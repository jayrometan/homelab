module github.com/jayrometan/homelab/scripts

go 1.22

// franz-go is the Redpanda-recommended Go Kafka client (written by twmb, whom
// Redpanda sponsors). Run `go mod tidy` once to fetch it and generate go.sum.
require github.com/twmb/franz-go v1.21.5
