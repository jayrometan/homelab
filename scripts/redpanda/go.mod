module github.com/jayrometan/homelab/scripts/redpanda

go 1.22

// franz-go is the Redpanda-recommended Go Kafka client (written by twmb, whom
// Redpanda sponsors). kadm is its admin sub-module (separate version), used by
// the metadata script. Run `go mod tidy` once to fetch them and write go.sum.
require (
	github.com/twmb/franz-go v1.21.5
	github.com/twmb/franz-go/pkg/kadm v1.18.0
)
