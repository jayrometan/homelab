// Command producer publishes random "order" messages to a Redpanda topic.
//
// It demonstrates the booklet's blessed producer posture (Vol 1 §7.1):
//   - acks=all            → the write is Raft quorum-committed before the ack
//                           (leader + 1 follower for RF=3), not just on the leader.
//   - idempotent producer → ON by default in franz-go; dedupes retries so a
//                           network hiccup can't duplicate a message.
//   - keyed records       → key = stock symbol, so all orders for a symbol hash
//                           to the same partition and keep per-key order.
//
// Run from your MacBook against the external NodePort listeners:
//   go run ./producer                       # forever, 1 msg / 500ms
//   go run ./producer -n 20 -interval 100ms # 20 messages, faster
//   go run ./producer -topic demo.orders.v1 -brokers 192.168.1.25:31092
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Order is the random payload we publish, serialized as JSON.
type Order struct {
	OrderID string    `json:"order_id"`
	Symbol  string    `json:"symbol"`
	Side    string    `json:"side"`
	Qty     int       `json:"qty"`
	Price   float64   `json:"price"`
	TS      time.Time `json:"ts"`
}

var (
	symbols = []string{"AAPL", "MSFT", "GOOG", "AMZN", "TSLA", "NVDA", "META"}
	sides   = []string{"BUY", "SELL"}
)

func main() {
	brokers := flag.String("brokers",
		"192.168.1.25:31092,192.168.1.26:31092,192.168.1.27:31092",
		"comma-separated bootstrap brokers (the external NodePort addresses)")
	topic := flag.String("topic", "demo.orders.v1", "topic to produce to")
	n := flag.Int("n", 0, "number of messages to send (0 = run until Ctrl-C)")
	interval := flag.Duration("interval", 500*time.Millisecond, "delay between messages")
	flag.Parse()

	cl, err := kgo.NewClient(
		kgo.SeedBrokers(strings.Split(*brokers, ",")...),
		kgo.DefaultProduceTopic(*topic),
		// acks=all: return only after a majority of the Raft group is durable.
		kgo.RequiredAcks(kgo.AllISRAcks()),
		// Latency-first: send each record immediately rather than batching.
		// Bump this (e.g. 5ms) + add compression for a throughput profile.
		kgo.ProducerLinger(0),
		// (idempotency is on by default and is compatible with acks=all)
	)
	if err != nil {
		log.Fatalf("create client: %v", err)
	}
	defer cl.Close()

	// Ctrl-C → cancel the context → clean shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() { <-sigs; log.Println("shutting down…"); cancel() }()

	log.Printf("producing to %q via %s (acks=all, idempotent) — Ctrl-C to stop", *topic, *brokers)

	sent := 0
	for *n == 0 || sent < *n {
		if ctx.Err() != nil {
			break
		}

		o := Order{
			OrderID: fmt.Sprintf("ord-%d", rand.Int63()),
			Symbol:  symbols[rand.Intn(len(symbols))],
			Side:    sides[rand.Intn(len(sides))],
			Qty:     (rand.Intn(100) + 1) * 10,
			Price:   50 + rand.Float64()*450,
			TS:      time.Now().UTC(),
		}
		val, _ := json.Marshal(o)

		rec := &kgo.Record{
			Key:   []byte(o.Symbol), // partition by symbol → per-symbol ordering
			Value: val,
		}

		// ProduceSync blocks until the broker acks (quorum-committed). Simple and
		// clear for a demo; a high-throughput producer would use async Produce
		// with a callback and let franz-go batch in flight.
		if err := cl.ProduceSync(ctx, rec).FirstErr(); err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Printf("produce error: %v", err)
			continue
		}

		log.Printf("→ partition %d  offset %d  key=%-5s  %s",
			rec.Partition, rec.Offset, o.Symbol, string(val))
		sent++

		select {
		case <-ctx.Done():
		case <-time.After(*interval):
		}
	}

	log.Printf("done — produced %d messages", sent)
}
