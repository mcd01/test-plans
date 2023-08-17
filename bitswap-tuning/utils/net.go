package utils

import (
	"context"
	"github.com/testground/sdk-go/network"
	"strings"
	"time"

	"github.com/testground/sdk-go/runtime"
)

// SetupNetwork instructs the sidecar (if enabled) to setup the network for this
// test case.
func SetupNetwork(ctx context.Context, runEnv *runtime.RunEnv, netClient *network.Client,
	nodeTp NodeType, tpIndex int) (time.Duration, int, error) {

	if !runEnv.TestSidecar {
		return 0, 0, nil
	}

	// instantiate a network client; see 'Traffic shaping' in the docs.
	// Wait for the network to be initialized.
	netClient.MustWaitNetworkInitialized(ctx)

	latency, err := getLatency(runEnv, nodeTp, tpIndex)
	if err != nil {
		return 0, 0, err
	}

	jitterPct := runEnv.IntParam("jitter_pct")
	bandwidth := runEnv.IntParam("bandwidth_mb")

	netClient.MustConfigureNetwork(ctx, &network.Config{
		Network: "default",
		Enable:  true,
		// Set the traffic shaping characteristics.
		Default: network.LinkShape{
			Latency:   latency,
			Jitter:    (time.Duration(jitterPct) * latency) / 100,
			Bandwidth: uint64(bandwidth) * 1024 * 1024,
		},
		CallbackState:  "network-configured",
		CallbackTarget: runEnv.TestInstanceCount,
	})

	runEnv.RecordMessage("%s %d has %s latency (%d%% jitter) and %dMB bandwidth", nodeTp, tpIndex, latency, jitterPct, bandwidth)

	return latency, bandwidth, nil
}

// If there's a latency specific to the node type, overwrite the default latency
func getLatency(runEnv *runtime.RunEnv, nodeTp NodeType, tpIndex int) (time.Duration, error) {
	latency := time.Duration(runEnv.IntParam("latency_ms")) * time.Millisecond
	var err error
	if nodeTp == Seed {
		latency, err = getTypeLatency(runEnv, "seed_latency_ms", tpIndex, latency)
	} else if nodeTp == Leech {
		latency, err = getTypeLatency(runEnv, "leech_latency_ms", tpIndex, latency)
	}
	if err != nil {
		return 0, err
	}
	return latency, nil
}

// If the parameter is a comma-separated list, each value in the list
// corresponds to the type index. For example:
// seed_latency_ms=100,200,400
// means that
// - the first seed has 100ms latency
// - the second seed has 200ms latency
// - the third seed has 400ms latency
// - any subsequent seeds have defaultLatency
func getTypeLatency(runEnv *runtime.RunEnv, param string, tpIndex int, defaultLatency time.Duration) (time.Duration, error) {
	// No type specific latency set, just return the default
	if !runEnv.IsParamSet(param) {
		return defaultLatency, nil
	}

	// Not a comma-separated list, interpret the value as an int and apply
	// the same latency to all peers of this type
	if !strings.Contains(runEnv.StringParam(param), ",") {
		return time.Duration(runEnv.IntParam(param)) * time.Millisecond, nil
	}

	// Comma separated list, the position in the list corresponds to the
	// type index
	latencies, err := ParseIntArray(runEnv.StringParam(param))
	if err != nil {
		return 0, err
	}
	if tpIndex < len(latencies) {
		return time.Duration(latencies[tpIndex]) * time.Millisecond, nil
	}

	// More peers of this type than entries in the list. Return the default
	// latency for peers not covered by list entries
	return defaultLatency, nil
}
