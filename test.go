package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/cometbft/cometbft/libs/log"

	"github.com/cometbft/cometbft/crypto"

	cfg "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/node"

	"golang.org/x/sync/errgroup"

	"github.com/testground/sdk-go/network"
	"github.com/testground/sdk-go/run"
	"github.com/testground/sdk-go/runtime"
	tgsync "github.com/testground/sdk-go/sync"
)

const (
	DefaultTestChainID = "test-chain"
)

type IP struct {
	Address net.IP
}

type Multiaddr struct {
	Addr string
	Ip   string
	Port string
}

type Genesis struct {
	Gen []byte
}

type ValidatorKey struct {
	ValKey []byte
}

type KV struct {
	Kv int
}

type NodeKey struct {
	PrivKey crypto.PrivKey `json:"priv_key"` // our priv key
}

// setupNetwork instructs the sidecar (if enabled) to setup the network for this
// test case.
func setupNetwork(ctx context.Context, runenv *runtime.RunEnv, netclient *network.Client, latencyMin int, latencyMax int, bandwidth int) (*network.Config, error) {
	if !runenv.TestSidecar {
		return nil, nil
	}

	// Wait for the network to be initialized.
	runenv.RecordMessage("Waiting for network initialization")
	err := netclient.WaitNetworkInitialized(ctx)
	if err != nil {
		return nil, err
	}
	runenv.RecordMessage("Network init complete")

	lat := rand.Intn(latencyMax-latencyMin) + latencyMin

	bw := uint64(bandwidth) * 1000 * 1000

	runenv.RecordMessage("Network params %d %d", lat, bw)

	config := &network.Config{
		Network: "default",
		Enable:  true,
		Default: network.LinkShape{
			Latency:   time.Duration(lat) * time.Millisecond,
			Bandwidth: bw, //Equivalent to 100Mps
		},
		CallbackState: "network-configured",
		RoutingPolicy: network.DenyAll,
	}

	// random delay to avoid overloading weave (we hope)
	//delay := time.Duration(rand.Intn(1000)) * time.Millisecond
	//<-time.After(delay)
	err = netclient.ConfigureNetwork(ctx, config)
	if err != nil {
		return nil, err
	}

	return config, nil
}

func test(runenv *runtime.RunEnv, initCtx *run.InitContext) error {
	params := parseParams(runenv)

	setup := params.setup
	warmup := params.warmup
	cooldown := params.cooldown
	runTime := params.runtime
	totalTime := setup + runTime + warmup + cooldown

	ctx, cancel := context.WithTimeout(context.Background(), totalTime)
	defer cancel()

	client := tgsync.MustBoundClient(ctx, runenv)
	defer client.Close()

	netclient := network.NewClient(client, runenv)

	test := tgsync.NewTopic("test", &IP{})
	seq, err := client.Publish(ctx, test, &IP{})
	if err != nil {
		return fmt.Errorf("failed to set publish: %w", err)
	}
	runenv.RecordMessage("my sequence ID: %d ", seq)

	bw := params.netParams.bandwidthMB
	//if int(seq) == runenv.TestInstanceCount-1 || int(seq) == runenv.TestInstanceCount {
	if int(seq) != 2 {
		bw = bw * 1000
	}
	_, err = setupNetwork(ctx, runenv, netclient, params.netParams.latency, params.netParams.latencyMax, bw)
	if err != nil {
		return fmt.Errorf("failed to set up network: %w", err)
	}

	netclient.MustWaitNetworkInitialized(ctx)

	peers := tgsync.NewTopic("nodes", &IP{})

	// Get sequence number within a node type (eg honest-1, honest-2, etc)
	// signal entry in the 'enrolled' state, and obtain a sequence number.
	ip, _ := netclient.GetDataNetworkIP()
	runenv.RecordMessage("ip: %s", ip)
	if seq == 1 {
		_, err = client.Publish(ctx, peers, &IP{ip})
		if err != nil {
			return fmt.Errorf("failed to publish: %w", err)
		}
	} else {
		tch := make(chan *IP)
		client.Subscribe(ctx, peers, tch)
		t := <-tch
		runenv.RecordMessage("ip received: %s ", t.Address)
	}
	config := createConfig("test1", DefaultTestChainID)
	runenv.RecordMessage("new config")

	errgrp, _ := errgroup.WithContext(ctx)

	errgrp.Go(func() (err error) {
		err = startNode(runenv, config, runTime)
		if err != nil {
			runenv.RecordMessage("err: %s", err)
		}
		runenv.RecordMessage("node started")
		return
	})
	return errgrp.Wait()

}

func startNode(runenv *runtime.RunEnv, config *cfg.Config, runtime time.Duration) error {

	runenv.RecordMessage("starting node")

	logger := log.NewTMLogger(os.Stdout)

	n, err := node.DefaultNewNode(config, logger, node.CliParams{}, nil)

	/*n, err := node.NewNode(context.Background(),
		config,
		pv,
		nodeKey,
		proxy.DefaultClientCreator(config.ProxyApp, config.ABCI, config.DBDir()),
		node.DefaultGenesisDocProviderFunc(config),
		cfg.DefaultDBProvider,
		node.DefaultMetricsProvider(config.Instrumentation),
		nil,
	)*/
	if err != nil {
		return err
	}

	n.Start()

	select {
	case <-time.After(runtime):
		return errors.New("timeout")
	}

}

func createConfig(testName string, chainID string) *cfg.Config {
	// create a unique, concurrency-safe test directory under os.TempDir()
	//rootDir, err := os.MkdirTemp("", fmt.Sprintf("%s-%s_", chainID, testName))
	rootDir := fmt.Sprintf("/%s-%s_", chainID, testName)
	err := os.Mkdir(rootDir, os.ModePerm)
	if err != nil {
		panic(err)
	}

	cfg.EnsureRoot(rootDir)

	baseConfig := cfg.DefaultBaseConfig()
	genesisFilePath := filepath.Join(rootDir, baseConfig.Genesis)
	privKeyFilePath := filepath.Join(rootDir, baseConfig.PrivValidatorKey)
	privStateFilePath := filepath.Join(rootDir, baseConfig.PrivValidatorState)

	testGenesis := fmt.Sprintf(testGenesisFmt, chainID)
	os.WriteFile(genesisFilePath, []byte(testGenesis), 0o644)

	os.WriteFile(privKeyFilePath, []byte(testPrivValidatorKey), 0o644)
	os.WriteFile(privStateFilePath, []byte(testPrivValidatorState), 0o644)

	config := cfg.TestConfig().SetRoot(rootDir)
	return config
}

var testGenesisFmt = `{
  "genesis_time": "2018-10-10T08:20:13.695936996Z",
  "chain_id": "%s",
  "initial_height": "1",
  "consensus_params": {
		"block": {
			"max_bytes": "22020096",
			"max_gas": "-1",
			"time_iota_ms": "10"
		},
		"synchrony": {
			"message_delay": "500000000",
			"precision": "10000000"
		},
		"evidence": {
			"max_age_num_blocks": "100000",
			"max_age_duration": "172800000000000",
			"max_bytes": "1048576"
		},
		"validator": {
			"pub_key_types": [
				"ed25519"
			]
		},
		"abci": {
			"vote_extensions_enable_height": "0"
		},
		"version": {},
		"feature": {
			"vote_extensions_enable_height": "0",
			"pbts_enable_height": "1"
		}
  },
  "validators": [
    {
      "pub_key": {
        "type": "tendermint/PubKeyEd25519",
        "value":"AT/+aaL1eB0477Mud9JMm8Sh8BIvOYlPGC9KkIUmFaE="
      },
      "power": "10",
      "name": ""
    }
  ],
  "app_hash": ""
}`

var testPrivValidatorKey = `{
  "address": "A3258DCBF45DCA0DF052981870F2D1441A36D145",
  "pub_key": {
    "type": "tendermint/PubKeyEd25519",
    "value": "AT/+aaL1eB0477Mud9JMm8Sh8BIvOYlPGC9KkIUmFaE="
  },
  "priv_key": {
    "type": "tendermint/PrivKeyEd25519",
    "value": "EVkqJO/jIXp3rkASXfh9YnyToYXRXhBr6g9cQVxPFnQBP/5povV4HTjvsy530kybxKHwEi85iU8YL0qQhSYVoQ=="
  }
}`

var testPrivValidatorState = `{
  "height": "0",
  "round": 0,
  "step": 0
}`

// Returns data network addresses of test peer instances
/*func getPeerAddrs(ctx context.Context, runenv *runtime.RunEnv, ownDataIp net.IP, client sync.Client) ([]string, error) {
	_ = client.MustSignalAndWait(ctx, sync.State("listening"), runenv.TestInstanceCount)

	peerAddrs, err := exchangeAddrWithPeers(ctx, client, runenv, ownDataIp.String())
	if err != nil {
		return nil, err
	}

	_ = client.MustSignalAndWait(ctx, sync.State("got-other-addrs"), runenv.TestInstanceCount)

	return peerAddrs, nil
}

// Shares this instance's address with all other instances in the test,
//
//	collects all other instances' addresses and returns them
func exchangeAddrWithPeers(ctx context.Context, client sync.Client, runenv *runtime.RunEnv, addr string) ([]string, error) {
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	peerTopic := sync.NewTopic("peers", "")
	ch := make(chan string)
	if _, _, err := client.PublishSubscribe(subCtx, peerTopic, addr, ch); err != nil {
		return nil, fmt.Errorf(err.Error())
	}

	res := []string{}

	for i := 0; i < runenv.TestInstanceCount; i++ {
		select {
		case otherAddr := <-ch:
			if addr != otherAddr {
				res = append(res, otherAddr)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		runenv.D().Counter("got.info").Inc(1)
	}

	return res, nil
}

func sameAddrs(a, b []net.Addr) bool {
	if len(a) != len(b) {
		return false
	}
	aset := make(map[string]bool, len(a))
	for _, addr := range a {
		aset[addr.String()] = true
	}
	for _, addr := range b {
		if !aset[addr.String()] {
			return false
		}
	}
	return true
}*/
