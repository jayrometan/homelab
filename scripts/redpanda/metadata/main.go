// Command metadata queries a Redpanda/Kafka broker for cluster metadata and
// prints it: the cluster id, the controller, every broker with its ADVERTISED
// address, and every topic's partitions (leader / replicas / in-sync set).
//
// This is the exact information a Kafka client receives at bootstrap (Vol 1
// §6.1): you connect to ONE address, and the broker hands back the addresses of
// ALL brokers plus which one leads each partition. Running this against each of
// your external addresses is the fastest way to debug advertised-listener
// problems — the "connects then times out" class of bug (Vol 2 §15.4): if the
// HOST column here shows addresses your client can't reach, that's the bug.
//
// The bootstrap address is a REQUIRED positional argument:
//
//	go run ./metadata 192.168.1.25:31092
//	go run ./metadata 192.168.1.27:31094 -topic demo.orders.v1
//	go run ./metadata redpanda-0.redpanda.redpanda.svc.cluster.local.:9093  # from in-cluster
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

func main() {
	// -topic is optional; positional arg 0 is the required ip:port.
	topic := flag.String("topic", "", "only show this topic (default: all topics)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: metadata <ip:port> [-topic NAME]\n\n")
		fmt.Fprintf(os.Stderr, "  <ip:port>  REQUIRED bootstrap broker address to query\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	addr := flag.Arg(0)

	cl, err := kgo.NewClient(kgo.SeedBrokers(addr))
	if err != nil {
		die("create client: %v", err)
	}
	defer cl.Close()

	adm := kadm.NewClient(cl)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// No topic args → metadata for every topic. With a filter, just that one.
	var topics []string
	if *topic != "" {
		topics = []string{*topic}
	}
	meta, err := adm.Metadata(ctx, topics...)
	if err != nil {
		die("fetch metadata from %s: %v", addr, err)
	}

	fmt.Printf("Queried:    %s\n", addr)
	fmt.Printf("Cluster:    %s\n", meta.Cluster)
	fmt.Printf("Controller: %d\n\n", meta.Controller)

	// ── Brokers: the advertised addresses clients will actually dial ──────────
	fmt.Println("BROKERS")
	bw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(bw, "  ID\tHOST\tPORT\tRACK")
	brokers := meta.Brokers // []BrokerDetail
	sort.Slice(brokers, func(i, j int) bool { return brokers[i].NodeID < brokers[j].NodeID })
	for _, b := range brokers {
		rack := ""
		if b.Rack != nil {
			rack = *b.Rack
		}
		marker := " "
		if b.NodeID == meta.Controller {
			marker = "*" // controller leader
		}
		fmt.Fprintf(bw, "%s %d\t%s\t%d\t%s\n", marker, b.NodeID, b.Host, b.Port, rack)
	}
	bw.Flush()

	// ── Topics: partitions with leader / replicas / in-sync replicas ──────────
	fmt.Println("\nTOPICS")
	names := make([]string, 0, len(meta.Topics))
	for name := range meta.Topics {
		names = append(names, name)
	}
	sort.Strings(names)

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  TOPIC\tPART\tLEADER\tREPLICAS\tISR")
	for _, name := range names {
		td := meta.Topics[name]
		parts := make([]int32, 0, len(td.Partitions))
		for p := range td.Partitions {
			parts = append(parts, p)
		}
		sort.Slice(parts, func(i, j int) bool { return parts[i] < parts[j] })
		for _, p := range parts {
			pd := td.Partitions[p]
			fmt.Fprintf(tw, "  %s\t%d\t%d\t%v\t%v\n",
				name, pd.Partition, pd.Leader, pd.Replicas, pd.ISR)
		}
	}
	tw.Flush()
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
