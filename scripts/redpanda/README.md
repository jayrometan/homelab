# Redpanda client scripts (Go / franz-go)

Three small Go programs for the homelab Redpanda cluster, meant to be run **from
your MacBook** against the external NodePort listeners:

- `producer/` — publish random order messages
- `consumer/` — read messages as a consumer group
- `metadata/` — query a broker for cluster metadata (brokers + topics)

## Brokers (external NodePort access)

```
192.168.1.25:31092   → redpanda-0 (jay1)
192.168.1.26:31093   → redpanda-1 (jay2)
192.168.1.27:31094   → redpanda-2 (jay3)
```

These are the defaults baked into both scripts. Plaintext, no auth (milestone 1).

## One-time setup

```bash
cd scripts/redpanda
go mod tidy          # fetches franz-go + kadm and writes go.sum
```

## Produce random order messages

```bash
go run ./producer                        # 1 msg / 500ms, forever (Ctrl-C to stop)
go run ./producer -n 20 -interval 100ms  # 20 messages, 10/sec
go run ./producer -topic demo.orders.v1
```

Each message is a random JSON order, keyed by stock symbol (so all orders for a
symbol land on the same partition — per-key ordering). Published with `acks=all`
(Raft quorum commit) and the idempotent producer on.

## Consume messages

```bash
go run ./consumer                 # from the tip: only messages sent from now on
go run ./consumer -from start     # replay the whole topic from offset 0
go run ./consumer -group my.reader -topic demo.orders.v1
```

### See a consumer group rebalance

1. Terminal A: `go run ./consumer`
2. Terminal B: `go run ./producer`
3. Terminal C: `go run ./consumer` (same default `-group`)

The 6 partitions split across the two consumer instances via cooperative
(incremental) rebalancing. Kill one and watch its partitions move to the other.

## Query broker metadata

Takes a **required `ip:port`** argument — the broker to bootstrap against. Prints
the cluster id, controller, every broker's *advertised* address, and each topic's
partitions (leader / replicas / ISR). This is exactly what a client receives at
bootstrap, so it's the fastest way to debug advertised-listener issues (Vol 2
§15.4): if the HOST column shows addresses your client can't reach, that's the bug.

```bash
go run ./metadata 192.168.1.25:31092                    # via broker 0 (jay1)
go run ./metadata 192.168.1.27:31094 -topic demo.orders.v1
go run ./metadata redpanda-0.redpanda.redpanda.svc.cluster.local.:9093  # in-cluster
```

Try it against each external address — every broker should report the *same* set
of advertised broker addresses (`.25:31092`, `.26:31093`, `.27:31094`).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-brokers` | the 3 external addresses | comma-separated bootstrap list |
| `-topic` | `demo.orders.v1` | topic name |
| `-n` (producer) | `0` | messages to send (0 = forever) |
| `-interval` (producer) | `500ms` | delay between messages |
| `-group` (consumer) | `demo.orders.reader` | consumer group id |
| `-from` (consumer) | `end` | new-group start point: `start` or `end` |

## Verifying from the cluster side

```bash
# topic + per-partition offsets
kubectl exec -n redpanda redpanda-0 -c redpanda -- rpk topic describe demo.orders.v1 -p

# consumer group lag (the triage workhorse)
kubectl exec -n redpanda redpanda-0 -c redpanda -- rpk group describe demo.orders.reader
```

Or open the Console UI at <http://192.168.1.243:8080> → Topics / Consumer Groups.

> When we enable SASL + TLS (lesson 2), these scripts get a couple of extra
> `kgo` options (`kgo.SASL(...)`, `kgo.DialTLSConfig(...)`) and the brokers move
> to the TLS port — the rest stays the same.
