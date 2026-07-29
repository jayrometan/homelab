// Command consumer reads messages from a Redpanda topic as part of a consumer
// group and prints them.
//
// It demonstrates the booklet's blessed consumer posture (Vol 1 §7.2):
//   - consumer group        → partitions are distributed across all instances
//                             of the group; run N copies and they share the work.
//   - cooperative-sticky     → incremental rebalancing (only moved partitions are
//     balancing               revoked), the blessed protocol that avoids the
//                             stop-the-world "rebalance storm".
//   - auto-commit            → franz-go commits offsets periodically and on
//                             shutdown; on crash you resume near where you left
//                             off (at-least-once — dedupe downstream if needed).
//
// Run from your MacBook against the external NodePort listeners:
//   go run ./consumer                          # from the tip (new messages only)
//   go run ./consumer -from start              # replay the whole topic
//   go run ./consumer -group demo.orders.reader -topic demo.orders.v1
//
// Tip: start the consumer, then run the producer in another terminal and watch
// messages arrive. Start a SECOND consumer with the same -group to see the 6
// partitions split between the two instances (cooperative rebalance).
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/twmb/franz-go/pkg/kgo"
)

func main() {
	brokers := flag.String("brokers",
		"192.168.1.25:31092,192.168.1.26:31093,192.168.1.27:31094",
		"comma-separated bootstrap brokers (the external NodePort addresses)")
	topic := flag.String("topic", "demo.orders.v1", "topic to consume")
	group := flag.String("group", "demo.orders.reader", "consumer group id")
	from := flag.String("from", "end", "where a NEW group starts reading: start|end")
	flag.Parse()

	// Only applies the first time this group commits offsets; after that the
	// committed offsets win and the group resumes from where it left off.
	reset := kgo.NewOffset().AtEnd()
	if *from == "start" {
		reset = kgo.NewOffset().AtStart()
	}

	cl, err := kgo.NewClient(
		kgo.SeedBrokers(strings.Split(*brokers, ",")...),
		kgo.ConsumerGroup(*group),
		kgo.ConsumeTopics(*topic),
		kgo.ConsumeResetOffset(reset),
		// Blessed rebalance protocol (Vol 1 §6.2): only reassign partitions that
		// must move, instead of every member dropping everything.
		kgo.Balancers(kgo.CooperativeStickyBalancer()),
	)
	if err != nil {
		log.Fatalf("create client: %v", err)
	}
	defer cl.Close()

	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() { <-sigs; log.Println("shutting down…"); cancel() }()

	log.Printf("consuming %q as group %q (new-group start=%s) — Ctrl-C to stop",
		*topic, *group, *from)

	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			break // context cancelled → shutting down
		}
		// Fetch-level errors (broker down, auth, etc.) surface here, per topic
		// and partition. Log and keep polling; franz-go retries under the hood.
		if errs := fetches.Errors(); len(errs) > 0 {
			for _, e := range errs {
				log.Printf("fetch error topic=%s partition=%d: %v", e.Topic, e.Partition, e.Err)
			}
			continue
		}

		fetches.EachRecord(func(r *kgo.Record) {
			log.Printf("← p%d @ %-6d key=%-5s  %s",
				r.Partition, r.Offset, string(r.Key), string(r.Value))
		})
	}

	log.Println("done")
}
