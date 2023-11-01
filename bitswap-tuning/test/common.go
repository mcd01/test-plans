package test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"

	"github.com/testground/sdk-go/runtime"
	"github.com/testground/sdk-go/sync"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/ipfs/test-plans/bitswap-tuning/utils"
)

func parseType(ctx context.Context, runEnv *runtime.RunEnv, client sync.Client, h host.Host, seq int64) (int64, utils.NodeType, int, error) {
	leechCount := runEnv.IntParam("leech_count")
	passiveCount := runEnv.IntParam("passive_count")

	grpCountOverride := false
	if runEnv.TestGroupID != "" {
		grpLchLabel := runEnv.TestGroupID + "_leech_count"
		if runEnv.IsParamSet(grpLchLabel) {
			leechCount = runEnv.IntParam(grpLchLabel)
			grpCountOverride = true
		}
		grpPsvLabel := runEnv.TestGroupID + "_passive_count"
		if runEnv.IsParamSet(grpPsvLabel) {
			passiveCount = runEnv.IntParam(grpPsvLabel)
			grpCountOverride = true
		}
	}

	var nodeTp utils.NodeType
	var tpIndex int
	grpSeq := seq
	seqStr := fmt.Sprintf("- seq %d / %d", seq, runEnv.TestInstanceCount)
	grpPrefix := ""
	if grpCountOverride {
		grpPrefix = runEnv.TestGroupID + " "

		var err error
		grpSeq, err = getNodeSetSeq(ctx, client, h, runEnv.TestGroupID)
		if err != nil {
			return grpSeq, nodeTp, tpIndex, err
		}

		seqStr = fmt.Sprintf("%s (%d / %d of %s)", seqStr, grpSeq, runEnv.TestGroupInstanceCount, runEnv.TestGroupID)
	}

	// Note: seq starts at 1 (not 0)
	switch {
	case grpSeq <= int64(leechCount):
		nodeTp = utils.Leech
		tpIndex = int(grpSeq) - 1
	case grpSeq > int64(leechCount+passiveCount):
		nodeTp = utils.Seed
		tpIndex = int(grpSeq) - 1 - (leechCount + passiveCount)
	default:
		nodeTp = utils.Passive
		tpIndex = int(grpSeq) - 1 - leechCount
	}

	runEnv.RecordMessage("I am %s %d %s", grpPrefix+nodeTp.String(), tpIndex, seqStr)

	return grpSeq, nodeTp, tpIndex, nil
}

func getNodeSetSeq(ctx context.Context, client sync.Client, h host.Host, setID string) (int64, error) {
	topic := sync.NewTopic("nodes"+setID, &peer.AddrInfo{})

	return client.Publish(ctx, topic, host.InfoFromHost(h))
}

func setupSeed(ctx context.Context, runEnv *runtime.RunEnv, node *utils.Node, fileSize int, seedIndex int) (cid.Cid, error) {
	tmpFile := utils.RandReader(fileSize)
	ipldNode, err := node.Add(ctx, tmpFile)
	if err != nil {
		return cid.Cid{}, err
	}

	if !runEnv.IsParamSet("seed_fraction") {
		return ipldNode.Cid(), nil
	}
	seedFrac := runEnv.StringParam("seed_fraction")
	if seedFrac == "" {
		return ipldNode.Cid(), nil
	}

	parts := strings.Split(seedFrac, "/")
	if len(parts) != 2 {
		return cid.Cid{}, fmt.Errorf("invalid seed fraction %s", seedFrac)
	}
	numerator, nErr := strconv.ParseInt(parts[0], 10, 64)
	denominator, dErr := strconv.ParseInt(parts[1], 10, 64)
	if nErr != nil || dErr != nil {
		return cid.Cid{}, fmt.Errorf("invalid seed fraction %s", seedFrac)
	}

	nodes, err := getLeafNodes(ctx, ipldNode, node.Dserv)
	if err != nil {
		return cid.Cid{}, err
	}
	var del []cid.Cid
	for i := 0; i < len(nodes); i++ {
		idx := i + seedIndex
		if idx%int(denominator) >= int(numerator) {
			del = append(del, nodes[i].Cid())
		}
	}
	if err := node.Dserv.RemoveMany(ctx, del); err != nil {
		return cid.Cid{}, err
	}

	runEnv.RecordMessage("Retained %d / %d of blocks from seed, removed %d / %d blocks", numerator, denominator, len(del), len(nodes))
	return ipldNode.Cid(), nil
}

func getLeafNodes(ctx context.Context, node ipld.Node, dServ ipld.DAGService) ([]ipld.Node, error) {
	if len(node.Links()) == 0 {
		return []ipld.Node{node}, nil
	}

	var leaves []ipld.Node
	for _, l := range node.Links() {
		child, err := l.GetNode(ctx, dServ)
		if err != nil {
			return nil, err
		}
		childLeaves, err := getLeafNodes(ctx, child, dServ)
		if err != nil {
			return nil, err
		}
		leaves = append(leaves, childLeaves...)
	}

	return leaves, nil
}

func getRootCidTopic(id int) *sync.Topic {
	return sync.NewTopic(fmt.Sprintf("root-cid-%d", id), &cid.Cid{})
}

func emitMetrics(runEnv *runtime.RunEnv, bsNode *utils.Node, runNum int, seq int64, grpSeq int64,
	latency time.Duration, bandwidthMB int, fileSize int, nodeTp utils.NodeType, tpIndex int, timeToFetch time.Duration, timeToValidate time.Duration) error {

	stats, err := bsNode.Bitswap.Stat()
	if err != nil {
		return fmt.Errorf("error getting stats from Bitswap: %w", err)
	}

	latencyMS := latency.Milliseconds()
	id := fmt.Sprintf("latencyMS:%d/bandwidthMB:%d/run:%d/seq:%d/groupName:%s/groupSeq:%d/fileSize:%d/nodeType:%s/nodeTypeIndex:%d",
		latencyMS, bandwidthMB, runNum, seq, runEnv.TestGroupID, grpSeq, fileSize, nodeTp, tpIndex)
	if nodeTp == utils.Leech {
		runEnv.R().RecordPoint(fmt.Sprintf("%s/name:time_to_fetch", id), float64(timeToFetch))
		runEnv.R().RecordPoint(fmt.Sprintf("%s/name:time_to_validate", id), float64(timeToValidate))
	}

	runEnv.R().RecordPoint(fmt.Sprintf("%s/name:msgs_rcvd", id), float64(stats.MessagesReceived))
	runEnv.R().RecordPoint(fmt.Sprintf("%s/name:data_sent", id), float64(stats.DataSent))
	runEnv.R().RecordPoint(fmt.Sprintf("%s/name:data_rcvd", id), float64(stats.DataReceived))
	runEnv.R().RecordPoint(fmt.Sprintf("%s/name:dup_data_rcvd", id), float64(stats.DupDataReceived))
	runEnv.R().RecordPoint(fmt.Sprintf("%s/name:blks_sent", id), float64(stats.BlocksSent))
	runEnv.R().RecordPoint(fmt.Sprintf("%s/name:blks_rcvd", id), float64(stats.BlocksReceived))
	runEnv.R().RecordPoint(fmt.Sprintf("%s/name:dup_blks_rcvd", id), float64(stats.DupBlksReceived))

	return nil
}
