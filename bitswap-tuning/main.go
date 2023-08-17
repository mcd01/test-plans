package main

import (
	"github.com/ipfs/test-plans/bitswap-tuning/test"
	"github.com/testground/sdk-go/run"
)

func main() {
	run.InvokeMap(map[string]interface{}{
		"transfer": test.Transfer,
		"fuzz":     test.Fuzz,
	})
}
