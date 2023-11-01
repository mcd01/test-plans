package test

import (
	"context"
	"fmt"
	"github.com/testground/sdk-go/network"
	"math/rand"
	"os"
	gort "runtime"
	"runtime/pprof"
	"strconv"
	"time"

	"github.com/ipfs/go-cid"
	"golang.org/x/sync/errgroup"

	"github.com/testground/sdk-go/runtime"
	"github.com/testground/sdk-go/sync"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"

	"github.com/ipfs/test-plans/bitswap-tuning/utils"
)

//
// To run use:
//
// ./testground run s bitswap-tuning/fuzz \
//   --builder=exec:go \
//   --runner="local:exec" \
//   --dep="github.com/ipfs/go-bitswap=master" \
//   -instances=8 \
//   --test-param cpuprof_path=/tmp/cpu.prof \
//   --test-param memprof_path=/tmp/mem.prof
//

// Fuzz test Bitswap
func Fuzz(runEnv *runtime.RunEnv) error {
	// Test Parameters
	timeout := time.Duration(runEnv.IntParam("timeout_secs")) * time.Second
	randomDisconnectsFq := float32(runEnv.IntParam("random_disconnects_fq")) / 100
	cpuProfilingEnabled := runEnv.IsParamSet("cpuprof_path")
	memProfilingEnabled := runEnv.IsParamSet("memprof_path")

	defaultMemProfileRate := gort.MemProfileRate
	if memProfilingEnabled {
		gort.MemProfileRate = 0
	}

	/// --- Set up
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	client := sync.MustBoundClient(ctx, runEnv)
	netClient := network.NewClient(client, runEnv)

	/// --- Tear down
	defer func() {
		_, err := client.SignalAndWait(ctx, "end", runEnv.TestInstanceCount)
		if err != nil {
			runEnv.RecordFailure(err)
		} else {
			runEnv.RecordSuccess()
		}
		err = client.Close()
		if err != nil {
			return
		}
	}()

	// Create libp2p node
	h, err := libp2p.New()
	if err != nil {
		return err
	}
	defer func(h host.Host) {
		err := h.Close()
		if err != nil {

		}
	}(h)

	peers := sync.NewTopic("peers", &peer.AddrInfo{})

	// Get sequence number of this host
	seq, err := client.Publish(ctx, peers, host.InfoFromHost(h))
	if err != nil {
		return err
	}

	// Get addresses of all peers
	peerCh := make(chan *peer.AddrInfo)
	sCtx, cancelSub := context.WithCancel(ctx)
	client.MustSubscribe(sCtx, peers, peerCh)

	addrInfos, err := utils.AddrInfosFromChan(peerCh, runEnv.TestInstanceCount)
	if err != nil {
		cancelSub()
		return fmt.Errorf("no addrs in %d seconds", timeout/time.Second)
	}
	cancelSub()

	/// --- Warm up

	runEnv.RecordMessage("I am %s with addrs: %v", h.ID(), h.Addrs())

	// Set up network (with traffic shaping)
	err = setupFuzzNetwork(ctx, runEnv, netClient)
	if err != nil {
		return fmt.Errorf("failed to set up network: %w", err)
	}

	// Signal that this node is in the given state, and wait for all peers to
	// send the same signal
	signalAndWaitForAll := func(state string) error {
		_, err := client.SignalAndWait(ctx, sync.State(state), runEnv.TestInstanceCount)
		return err
	}

	// Wait for all nodes to be ready to start
	err = signalAndWaitForAll("start")
	if err != nil {
		return err
	}

	runEnv.RecordMessage("Starting")
	var bsNode *utils.Node
	rootCidTopic := sync.NewTopic("root-cid", &cid.Cid{})

	// Create a new blockstore
	bStoreDelay := 5 * time.Millisecond
	bStore, err := utils.CreateBlockstore(ctx, bStoreDelay)
	if err != nil {
		return err
	}

	// Create a new bitswap node from the blockstore
	bsNode, err = utils.CreateBitswapNode(ctx, h, bStore)
	if err != nil {
		return err
	}

	// Listen for seed generation
	rootCidCh := make(chan *cid.Cid, 1)
	sCtx, cancelRootCidSub := context.WithCancel(ctx)
	defer cancelRootCidSub()
	if _, err := client.Subscribe(sCtx, rootCidTopic, rootCidCh); err != nil {
		return fmt.Errorf("failed to subscribe to rootCidTopic %w", err)
	}

	seedGenerated := sync.State("seed-generated")
	var start time.Time
	// Each peer generates the seed data in series, to avoid
	// overloading a single machine hosting multiple instances
	seedIndex := seq - 1
	if seedIndex > 0 {
		// Wait for the seeds with an index lower than this one
		// to generate their seed data
		doneCh := client.MustBarrier(ctx, seedGenerated, int(seedIndex)).C
		if err = <-doneCh; err != nil {
			return err
		}
	}

	// Generate a file of random size and add it to the datastore
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	fileSize := 2*1024*1024 + rnd.Intn(64*1024*1024)
	runEnv.RecordMessage("Generating seed data of %d bytes", fileSize)
	start = time.Now()

	rootCid, err := setupSeed(ctx, runEnv, bsNode, fileSize, int(seedIndex))
	if err != nil {
		return fmt.Errorf("failed to set up seed: %w", err)
	}

	runEnv.RecordMessage("done generating seed data of %d bytes (%s)", fileSize, time.Since(start))

	// Signal we've completed generating the seed data
	_, err = client.SignalEntry(ctx, seedGenerated)
	if err != nil {
		return fmt.Errorf("failed to signal seed generated: %w", err)
	}

	// Inform other nodes of the root CID
	if _, err = client.Publish(ctx, rootCidTopic, &rootCid); err != nil {
		return fmt.Errorf("failed to get Redis Sync rootCidTopic %w", err)
	}

	// Get seed cid from all nodes
	var rootCids []cid.Cid
	for i := 0; i < runEnv.TestInstanceCount; i++ {
		select {
		case rootCidPtr := <-rootCidCh:
			rootCids = append(rootCids, *rootCidPtr)
		case <-time.After(timeout):
			return fmt.Errorf("could not get all cids in %d seconds", timeout/time.Second)
		}
	}
	cancelRootCidSub()

	// Wait for all nodes to be ready to dial
	err = signalAndWaitForAll("ready-to-connect")
	if err != nil {
		return err
	}

	// Dial all peers
	dialed, err := utils.DialOtherPeers(ctx, h, addrInfos)
	if err != nil {
		return err
	}
	runEnv.RecordMessage("Dialed %d other nodes", len(dialed))

	// Wait for all nodes to be connected
	err = signalAndWaitForAll("connect-complete")
	if err != nil {
		return err
	}

	/// --- Start test
	runEnv.RecordMessage("Start fetching")

	// Randomly disconnect and reconnect
	var cancelFetchingCtx func()
	if randomDisconnectsFq > 0 {
		var fetchingCtx context.Context
		fetchingCtx, cancelFetchingCtx = context.WithCancel(ctx)
		defer cancelFetchingCtx()
		go func() {
			for {
				time.Sleep(time.Duration(rnd.Intn(1000)) * time.Millisecond)

				select {
				case <-fetchingCtx.Done():
					return
				default:
					// One third of the time, disconnect from a peer then reconnect
					if rnd.Float32() < randomDisconnectsFq {
						connections := h.Network().Conns()
						conn := connections[rnd.Intn(len(connections))]
						runEnv.RecordMessage("    closing connection to %s", conn.RemotePeer())
						err := conn.Close()
						if err != nil {
							runEnv.RecordMessage("    error disconnecting: %w", err)
						} else {
							ai := peer.AddrInfo{
								ID:    conn.RemotePeer(),
								Addrs: []ma.Multiaddr{conn.RemoteMultiaddr()},
							}
							go func() {
								// time.Sleep(time.Duration(rnd.Intn(200)) * time.Millisecond)
								runEnv.RecordMessage("    reconnecting to %s", conn.RemotePeer())
								if err := h.Connect(fetchingCtx, ai); err != nil {
									runEnv.RecordMessage("    error while reconnecting to peer %v: %w", ai, err)
								}
								runEnv.RecordMessage("    reconnected to %s", conn.RemotePeer())
							}()
						}
					}
				}
			}
		}()
	}

	if cpuProfilingEnabled {
		f, err := os.Create(runEnv.StringParam("cpuprof_path") + "." + strconv.Itoa(int(seq)))
		if err != nil {
			return err
		}
		err = pprof.StartCPUProfile(f)
		if err != nil {
			return err
		}
	}
	if memProfilingEnabled {
		gort.MemProfileRate = defaultMemProfileRate
	}

	g, groupCtx := errgroup.WithContext(ctx)
	for _, rootCid := range rootCids {
		// Fetch two thirds of the root cids of other nodes
		if rnd.Float32() < 0.3 {
			continue
		}

		rootCid := rootCid
		g.Go(func() error {
			// Stagger the start of the fetch
			startDelay := time.Duration(rnd.Intn(10)) * time.Millisecond
			time.Sleep(startDelay)

			cidStr := rootCid.String()
			pretty := cidStr[len(cidStr)-6:]

			// Half the time do a regular fetch, half the time cancel and then
			// restart the fetch
			runEnv.RecordMessage("  FTCH %s after %s delay", pretty, startDelay)
			start = time.Now()
			cancelCtx, cancel := context.WithCancel(groupCtx)
			if rnd.Float32() < 0.5 {
				// Cancel after a delay
				go func() {
					cancelDelay := time.Duration(rnd.Intn(100)) * time.Millisecond
					time.Sleep(cancelDelay)
					runEnv.RecordMessage("  cancel %s after %s delay", pretty, startDelay)
					cancel()
				}()
				err = bsNode.FetchGraph(cancelCtx, rootCid)
				if err != nil {
					// If there was an error (probably because the fetch was
					// cancelled) try fetching again
					runEnv.RecordMessage("  got err fetching %s: %s", pretty, err)
					err = bsNode.FetchGraph(groupCtx, rootCid)
				}
			} else {
				defer cancel()
				err = bsNode.FetchGraph(cancelCtx, rootCid)
			}
			timeToFetch := time.Since(start)
			runEnv.RecordMessage("  RCVD %s in %s", pretty, timeToFetch)

			return err
		})
	}
	if err := g.Wait(); err != nil {
		return fmt.Errorf("error fetching data through Bitswap: %w", err)
	}
	runEnv.RecordMessage("Fetching complete")
	if randomDisconnectsFq > 0 {
		cancelFetchingCtx()
	}

	// Wait for all leeches to have downloaded the data from seeds
	err = signalAndWaitForAll("transfer-complete")
	if err != nil {
		return err
	}

	if cpuProfilingEnabled {
		pprof.StopCPUProfile()
	}
	if memProfilingEnabled {
		f, err := os.Create(runEnv.StringParam("memprof_path") + "." + strconv.Itoa(int(seq)))
		if err != nil {
			return err
		}
		err = pprof.WriteHeapProfile(f)
		if err != nil {
			return err
		}
		err = f.Close()
		if err != nil {
			return err
		}
	}

	// Shut down bitswap
	err = bsNode.Close()
	if err != nil {
		return fmt.Errorf("error closing Bitswap: %w", err)
	}

	// Disconnect peers
	for _, c := range h.Network().Conns() {
		err := c.Close()
		if err != nil {
			return fmt.Errorf("error disconnecting: %w", err)
		}
	}

	/// --- Ending the test

	return nil
}

// Set up traffic shaping with random latency and bandwidth
func setupFuzzNetwork(ctx context.Context, runEnv *runtime.RunEnv, netClient *network.Client) error {
	if !runEnv.TestSidecar {
		return nil
	}

	// Wait for the network to be initialized.
	netClient.MustWaitNetworkInitialized(ctx)

	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	latency := time.Duration(2+rnd.Intn(100)) * time.Millisecond
	bandwidth := 1 + rnd.Intn(100)

	netClient.MustConfigureNetwork(ctx, &network.Config{
		Network: "default",
		Enable:  true,
		// Set the traffic shaping characteristics.
		Default: network.LinkShape{
			Latency:   latency,
			Jitter:    (latency * 10) / 100,
			Bandwidth: uint64(bandwidth) * 1024 * 1024,
		},
		CallbackState:  "network-configured",
		CallbackTarget: runEnv.TestInstanceCount,
	})

	return nil
}
