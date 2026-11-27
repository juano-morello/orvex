# Single-engine hot-round baseline · 2026-11-26

Normal and near-deadline workloads passed all six trials at 80 players, the highest level tested. The same-ID retry workload passed all six trials at 40 players and four of six at 80. In the two missed trials the driver dispatched requests late. Scheduling p99 reached 10.2 ms for bets in the first sweep and 73.5 ms for retries in the second sweep. The runs establish a repeatability limit for this setup. They leave the engine's saturation point open.

Both sweeps used `make load-baseline` with the default workload, three rotated repeats each and a discarded warm-up. The second used the final reporting and reconciliation code. The rate and timing budgets were the same for both. The checkout included the baseline changes on top of `95669c6fede273ad7e8a1c94d30eb7f41ab8438b`. The driver, gateway, engine and Docker database shared Denver, a four-core Intel i5-3475S host with 15 GiB RAM and an S900-480 SSD. Other containers were running. Both sweeps used Go 1.26.11, PostgreSQL 18.6, Docker 29.9.3 and four engine pool connections.

For each trial, the acknowledgment statistic below is the largest successful HTTP acknowledgment p99 across command kinds. The median and range describe six such observations. The bracket also requires generator p99 of at most 10 ms per kind, acknowledgment p99 of at most 100 ms per kind, observed drain within one second of the last send, complete observations, reconciliation and a verified settled history.

| Workload | Players | Passing trials | Median acknowledgment statistic (ms) | Range (ms) |
|---|---:|---:|---:|---:|
| Normal | 10 | 6/6 | 17.3 | 9.4–34.4 |
| Normal | 40 | 6/6 | 16.5 | 14.1–30.5 |
| Normal | 80 | 6/6 | 24.0 | 11.8–29.6 |
| Deadline | 10 | 6/6 | 16.6 | 10.6–23.4 |
| Deadline | 40 | 6/6 | 25.2 | 14.4–29.2 |
| Deadline | 80 | 6/6 | 30.1 | 20.7–38.8 |
| Same-ID retry | 10 | 6/6 | 16.4 | 12.7–18.2 |
| Same-ID retry | 40 | 6/6 | 22.9 | 19.7–29.9 |
| Same-ID retry | 80 | 4/6 | 25.1 | 21.5–86.6 |

All 54 measured rounds settled and passed the v2 audit against the observed head. The 5,460 HTTP attempts included 780 same-ID retries. The gateway returned 5,437 durable receipts and 23 expected closed or sealed responses in the deadline workload. Execution delay uses the first sampled observation of the committed sequence. Pool and lock samples can miss short waits. Sustained throughput and independent-round concurrency require different workloads.

## MSI generator follow-up · 2026-11-27

MSI sent the requests over Tailscale while Denver ran the gateway, engine and PostgreSQL. A one-player retry smoke trial passed before the 80-player sweep. All six measured 80-player retry trials passed the same timing and correctness criteria. All 1,440 HTTP attempts received durable receipts, including 480 same-ID retries. All six rounds settled and passed audit. Across those trials the largest dispatch lag was 2.285 ms, the slowest acknowledgment was 30.759 ms and the longest observed last-send-to-drain interval was 20.9 ms. Each command kind has 80 attempts per trial, so its nearest-rank p99 is also its maximum.

The experiment establishes a passing 80-player retry burst for this two-machine setup. Higher burst sizes and sustained traffic remain unmeasured.
