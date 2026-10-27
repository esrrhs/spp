//go:build race

package proxy

// sppRaceWorkers scales down churn-style workloads under the race detector,
// where concurrent handshakes self-contend far more than they do in prod-like
// builds. Wedge detection (liveness gates) keeps its semantics: a healthy
// proxy still clears the gates easily in isolation.
const sppRaceWorkers = 8
