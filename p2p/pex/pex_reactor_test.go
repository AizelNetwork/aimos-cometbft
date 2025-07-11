package pex

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cosmos/gogoproto/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tmp2p "github.com/cometbft/cometbft/api/cometbft/p2p/v1"
	"github.com/cometbft/cometbft/v2/config"
	"github.com/cometbft/cometbft/v2/libs/log"
	"github.com/cometbft/cometbft/v2/p2p"
	"github.com/cometbft/cometbft/v2/p2p/mock"
	na "github.com/cometbft/cometbft/v2/p2p/netaddr"
	"github.com/cometbft/cometbft/v2/types"
)

var cfg *config.P2PConfig

func init() {
	cfg = config.DefaultP2PConfig()
	cfg.PexReactor = true
	cfg.AllowDuplicateIP = true
}

func TestPEXReactorBasic(t *testing.T) {
	r, book := createReactor(&ReactorConfig{})
	defer teardownReactor(book)

	assert.NotNil(t, r)
	assert.NotEmpty(t, r.StreamDescriptors())
}

func TestPEXReactorAddRemovePeer(t *testing.T) {
	r, book := createReactor(&ReactorConfig{})
	defer teardownReactor(book)

	size := book.Size()
	peer := p2p.CreateRandomPeer(false)

	r.AddPeer(peer)
	assert.Equal(t, size+1, book.Size())

	r.RemovePeer(peer, "peer not available")

	outboundPeer := p2p.CreateRandomPeer(true)

	r.AddPeer(outboundPeer)
	assert.Equal(t, size+1, book.Size(), "outbound peers should not be added to the address book")

	r.RemovePeer(outboundPeer, "peer not available")
}

// --- FAIL: TestPEXReactorRunning (11.10s)
//
//	pex_reactor_test.go:411: expected all switches to be connected to at
//	least one peer (switches: 0 => {outbound: 1, inbound: 0}, 1 =>
//	{outbound: 0, inbound: 1}, 2 => {outbound: 0, inbound: 0}, )
//
// EXPLANATION: peers are getting rejected because in switch#addPeer we check
// if any peer (who we already connected to) has the same IP. Even though local
// peers have different IP addresses, they all have the same underlying remote
// IP: 127.0.0.1.
func TestPEXReactorRunning(t *testing.T) {
	n := 3
	switches := make([]*p2p.Switch, n)

	// directory to store address books
	dir, err := os.MkdirTemp("", "pex_reactor")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	books := make([]AddrBook, n)
	logger := log.TestingLogger()

	// create switches
	for i := 0; i < n; i++ {
		switches[i] = p2p.MakeSwitch(cfg, i, func(i int, sw *p2p.Switch) *p2p.Switch {
			books[i] = NewAddrBook(filepath.Join(dir, fmt.Sprintf("addrbook%d.json", i)), false)
			books[i].SetLogger(logger.With("pex", i))
			sw.SetAddrBook(books[i])

			sw.SetLogger(logger.With("pex", i))

			r := NewReactor(books[i], &ReactorConfig{
				EnsurePeersPeriod: 250 * time.Millisecond,
			})
			r.SetLogger(logger.With("pex", i))
			sw.AddReactor("PEX", r)

			return sw
		})
	}

	addOtherNodeAddrToAddrBook := func(switchIndex, otherSwitchIndex int) {
		addr := switches[otherSwitchIndex].NetAddr()
		err := books[switchIndex].AddAddress(addr, addr)
		require.NoError(t, err)
	}

	addOtherNodeAddrToAddrBook(0, 1)
	addOtherNodeAddrToAddrBook(1, 0)
	addOtherNodeAddrToAddrBook(2, 1)

	for _, sw := range switches {
		err := sw.Start() // start switch and reactors
		require.NoError(t, err)
	}

	assertPeersWithTimeout(t, switches, 10*time.Second, n-1)

	// stop them
	for _, s := range switches {
		err := s.Stop()
		require.NoError(t, err)
	}
}

func TestPEXReactorReceive(t *testing.T) {
	r, book := createReactor(&ReactorConfig{})
	defer teardownReactor(book)

	peer := p2p.CreateRandomPeer(false)

	// we have to send a request to receive responses
	r.RequestAddrs(peer)

	size := book.Size()
	msg := &tmp2p.PexAddrs{Addrs: []tmp2p.NetAddress{peer.SocketAddr().ToProto()}}
	r.Receive(p2p.Envelope{ChannelID: PexChannel, Src: peer, Message: msg})
	assert.Equal(t, size+1, book.Size())

	r.Receive(p2p.Envelope{ChannelID: PexChannel, Src: peer, Message: &tmp2p.PexRequest{}})
}

func TestPEXReactorRequestMessageAbuse(t *testing.T) {
	r, book := createReactor(&ReactorConfig{})
	defer teardownReactor(book)

	sw := createSwitchAndAddReactors(r)
	sw.SetAddrBook(book)

	peer := mock.NewPeer(nil)
	peerAddr := peer.SocketAddr()
	p2p.AddPeerToSwitchPeerSet(sw, peer)
	assert.True(t, sw.Peers().Has(peer.ID()))
	err := book.AddAddress(peerAddr, peerAddr)
	require.NoError(t, err)
	require.True(t, book.HasAddress(peerAddr))

	id := peer.ID()

	// first time creates the entry
	r.Receive(p2p.Envelope{ChannelID: PexChannel, Src: peer, Message: &tmp2p.PexRequest{}})
	assert.True(t, r.lastReceivedRequests.Has(id))
	assert.True(t, sw.Peers().Has(peer.ID()))

	// next time sets the last time value
	r.Receive(p2p.Envelope{ChannelID: PexChannel, Src: peer, Message: &tmp2p.PexRequest{}})
	assert.True(t, r.lastReceivedRequests.Has(id))
	assert.True(t, sw.Peers().Has(peer.ID()))

	// third time is too many too soon - peer is removed
	r.Receive(p2p.Envelope{ChannelID: PexChannel, Src: peer, Message: &tmp2p.PexRequest{}})
	assert.False(t, r.lastReceivedRequests.Has(id))
	assert.False(t, sw.Peers().Has(peer.ID()))
	assert.True(t, book.IsBanned(peerAddr))
}

func TestPEXReactorAddrsMessageAbuse(t *testing.T) {
	r, book := createReactor(&ReactorConfig{})
	defer teardownReactor(book)

	sw := createSwitchAndAddReactors(r)
	sw.SetAddrBook(book)

	peer := mock.NewPeer(nil)
	p2p.AddPeerToSwitchPeerSet(sw, peer)
	assert.True(t, sw.Peers().Has(peer.ID()))

	id := peer.ID()

	// request addrs from the peer
	r.RequestAddrs(peer)
	assert.True(t, r.requestsSent.Has(id))
	assert.True(t, sw.Peers().Has(peer.ID()))

	msg := &tmp2p.PexAddrs{Addrs: []tmp2p.NetAddress{peer.SocketAddr().ToProto()}}

	// receive some addrs. should clear the request
	r.Receive(p2p.Envelope{ChannelID: PexChannel, Src: peer, Message: msg})
	assert.False(t, r.requestsSent.Has(id))
	assert.True(t, sw.Peers().Has(peer.ID()))

	// receiving more unsolicited addrs causes a disconnect and ban
	r.Receive(p2p.Envelope{ChannelID: PexChannel, Src: peer, Message: msg})
	assert.False(t, sw.Peers().Has(peer.ID()))
	assert.True(t, book.IsBanned(peer.SocketAddr()))
}

func TestCheckSeeds(t *testing.T) {
	// directory to store address books
	dir, err := os.MkdirTemp("", "pex_reactor")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	// 1. test creating peer with no seeds works
	peerSwitch := testCreateDefaultPeer(dir, 0)
	require.NoError(t, peerSwitch.Start())
	peerSwitch.Stop() //nolint:errcheck // ignore for tests

	// 2. create seed
	seed := testCreateSeed(dir, 1, []*na.NetAddr{}, []*na.NetAddr{})

	// 3. test create peer with online seed works
	peerSwitch = testCreatePeerWithSeed(dir, 2, seed)
	require.NoError(t, peerSwitch.Start())
	peerSwitch.Stop() //nolint:errcheck // ignore for tests

	// 4. test create peer with all seeds having unresolvable DNS fails
	badPeerConfig := &ReactorConfig{
		Seeds: []string{
			"ed3dfd27bfc4af18f67a49862f04cc100696e84d@bad.network.addr:26657",
			"d824b13cb5d40fa1d8a614e089357c7eff31b670@anotherbad.network.addr:26657",
		},
	}
	peerSwitch = testCreatePeerWithConfig(dir, 2, badPeerConfig)
	require.Error(t, peerSwitch.Start())
	peerSwitch.Stop() //nolint:errcheck // ignore for tests

	// 5. test create peer with one good seed address succeeds
	badPeerConfig = &ReactorConfig{
		Seeds: []string{
			"ed3dfd27bfc4af18f67a49862f04cc100696e84d@bad.network.addr:26657",
			"d824b13cb5d40fa1d8a614e089357c7eff31b670@anotherbad.network.addr:26657",
			seed.NetAddr().String(),
		},
	}
	peerSwitch = testCreatePeerWithConfig(dir, 2, badPeerConfig)
	require.NoError(t, peerSwitch.Start())
	peerSwitch.Stop() //nolint:errcheck // ignore for tests
}

func TestPEXReactorUsesSeedsIfNeeded(t *testing.T) {
	// directory to store address books
	dir, err := os.MkdirTemp("", "pex_reactor")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	// 1. create seed
	seed := testCreateSeed(dir, 0, []*na.NetAddr{}, []*na.NetAddr{})
	require.NoError(t, seed.Start())
	defer seed.Stop() //nolint:errcheck // ignore for tests

	// 2. create usual peer with only seed configured.
	peer := testCreatePeerWithSeed(dir, 1, seed)
	require.NoError(t, peer.Start())
	defer peer.Stop() //nolint:errcheck // ignore for tests

	// 3. check that the peer connects to seed immediately
	assertPeersWithTimeout(t, []*p2p.Switch{peer}, 3*time.Second, 1)
}

func TestConnectionSpeedForPeerReceivedFromSeed(t *testing.T) {
	// directory to store address books
	dir, err := os.MkdirTemp("", "pex_reactor")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	var id int
	var knownAddrs []*na.NetAddr

	// 1. Create some peers
	for id = 0; id < 3; id++ {
		peer := testCreateDefaultPeer(dir, id)
		require.NoError(t, peer.Start())
		addr := peer.NetAddr()
		defer peer.Stop() //nolint:errcheck // ignore for tests

		knownAddrs = append(knownAddrs, addr)
	}

	// 2. Create seed node which knows about the previous peers
	seed := testCreateSeed(dir, id, knownAddrs, knownAddrs)
	require.NoError(t, seed.Start())
	defer seed.Stop() //nolint:errcheck // ignore for tests

	// 3. Create a node with only seed configured.
	id++
	node := testCreatePeerWithSeed(dir, id, seed)
	require.NoError(t, node.Start())
	defer node.Stop() //nolint:errcheck // ignore for tests

	// 4. Check that the node connects to seed immediately
	assertPeersWithTimeout(t, []*p2p.Switch{node}, 3*time.Second, 1)

	// 5. Check that the node connects to the peers reported by the seed node
	assertPeersWithTimeout(t, []*p2p.Switch{node}, 10*time.Second, 2)

	// 6. Assert that the configured maximum number of inbound/outbound peers
	// are respected, see https://github.com/cometbft/cometbft/v2/issues/486
	outbound, inbound, dialing := node.NumPeers()
	assert.LessOrEqual(t, inbound, cfg.MaxNumInboundPeers)
	assert.LessOrEqual(t, outbound, cfg.MaxNumOutboundPeers)
	assert.LessOrEqual(t, dialing, cfg.MaxNumOutboundPeers+cfg.MaxNumInboundPeers-outbound-inbound)
}

func TestPEXReactorSeedMode(t *testing.T) {
	// directory to store address books
	dir, err := os.MkdirTemp("", "pex_reactor")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	pexRConfig := &ReactorConfig{SeedMode: true, SeedDisconnectWaitPeriod: 100 * time.Millisecond}
	pexR, book := createReactor(pexRConfig)
	defer teardownReactor(book)

	sw := createSwitchAndAddReactors(pexR)
	sw.SetAddrBook(book)
	err = sw.Start()
	require.NoError(t, err)
	defer sw.Stop() //nolint:errcheck // ignore for tests

	assert.Zero(t, sw.Peers().Size())

	peerSwitch := testCreateDefaultPeer(dir, 1)
	require.NoError(t, peerSwitch.Start())
	defer peerSwitch.Stop() //nolint:errcheck // ignore for tests

	// 1. Test crawlPeers dials the peer
	pexR.crawlPeers([]*na.NetAddr{peerSwitch.NetAddr()})
	assert.Equal(t, 1, sw.Peers().Size())
	assert.True(t, sw.Peers().Has(peerSwitch.NodeInfo().ID()))

	// 2. attemptDisconnects should not disconnect because of wait period
	pexR.attemptDisconnects()
	assert.Equal(t, 1, sw.Peers().Size())

	// sleep for SeedDisconnectWaitPeriod
	time.Sleep(pexRConfig.SeedDisconnectWaitPeriod + 1*time.Millisecond)

	// 3. attemptDisconnects should disconnect after wait period
	pexR.attemptDisconnects()
	assert.Equal(t, 0, sw.Peers().Size())
}

func TestPEXReactorDoesNotDisconnectFromPersistentPeerInSeedMode(t *testing.T) {
	// directory to store address books
	dir, err := os.MkdirTemp("", "pex_reactor")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	pexRConfig := &ReactorConfig{SeedMode: true, SeedDisconnectWaitPeriod: 1 * time.Millisecond}
	pexR, book := createReactor(pexRConfig)
	defer teardownReactor(book)

	sw := createSwitchAndAddReactors(pexR)
	sw.SetAddrBook(book)
	err = sw.Start()
	require.NoError(t, err)
	defer sw.Stop() //nolint:errcheck // ignore for tests

	assert.Zero(t, sw.Peers().Size())

	peerSwitch := testCreateDefaultPeer(dir, 1)
	require.NoError(t, peerSwitch.Start())
	defer peerSwitch.Stop() //nolint:errcheck // ignore for tests

	err = sw.AddPersistentPeers([]string{peerSwitch.NetAddr().String()})
	require.NoError(t, err)

	// 1. Test crawlPeers dials the peer
	pexR.crawlPeers([]*na.NetAddr{peerSwitch.NetAddr()})
	assert.Equal(t, 1, sw.Peers().Size())
	assert.True(t, sw.Peers().Has(peerSwitch.NodeInfo().ID()))

	// sleep for SeedDisconnectWaitPeriod
	time.Sleep(pexRConfig.SeedDisconnectWaitPeriod + 1*time.Millisecond)

	// 2. attemptDisconnects should not disconnect because the peer is persistent
	pexR.attemptDisconnects()
	assert.Equal(t, 1, sw.Peers().Size())
}

func TestPEXReactorDialsPeerUpToMaxAttemptsInSeedMode(t *testing.T) {
	// directory to store address books
	dir, err := os.MkdirTemp("", "pex_reactor")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	pexR, book := createReactor(&ReactorConfig{SeedMode: true})
	defer teardownReactor(book)

	sw := createSwitchAndAddReactors(pexR)
	sw.SetAddrBook(book)
	// No need to start sw since crawlPeers is called manually here.

	peer := mock.NewPeer(nil)
	addr := peer.SocketAddr()

	err = book.AddAddress(addr, addr)
	require.NoError(t, err)

	assert.True(t, book.HasAddress(addr))

	// imitate maxAttemptsToDial reached
	pexR.attemptsToDial.Store(addr.DialString(), _attemptsToDial{maxAttemptsToDial + 1, time.Now()})
	pexR.crawlPeers([]*na.NetAddr{addr})

	assert.False(t, book.HasAddress(addr))
}

// connect a peer to a seed, wait a bit, then stop it.
// this should give it time to request addrs and for the seed
// to call FlushStop, and allows us to test calling Stop concurrently
// with FlushStop. Before a fix, this non-deterministically reproduced
// https://github.com/tendermint/tendermint/issues/3231.
func TestPEXReactorSeedModeFlushStop(t *testing.T) {
	n := 2
	switches := make([]*p2p.Switch, n)

	// directory to store address books
	dir, err := os.MkdirTemp("", "pex_reactor")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	books := make([]AddrBook, n)
	logger := log.TestingLogger()

	// create switches
	for i := 0; i < n; i++ {
		switches[i] = p2p.MakeSwitch(cfg, i, func(i int, sw *p2p.Switch) *p2p.Switch {
			books[i] = NewAddrBook(filepath.Join(dir, fmt.Sprintf("addrbook%d.json", i)), false)
			books[i].SetLogger(logger.With("pex", i))
			sw.SetAddrBook(books[i])

			sw.SetLogger(logger.With("pex", i))

			config := &ReactorConfig{}
			if i == 0 {
				// first one is a seed node
				config = &ReactorConfig{
					SeedMode:          true,
					EnsurePeersPeriod: 250 * time.Millisecond,
				}
			}
			r := NewReactor(books[i], config)
			r.SetLogger(logger.With("pex", i))
			sw.AddReactor("pex", r)

			return sw
		})
	}

	for _, sw := range switches {
		err := sw.Start() // start switch and reactors
		require.NoError(t, err)
	}

	reactor := switches[0].Reactors()["pex"].(*Reactor)
	peerID := switches[1].NodeInfo().ID()

	err = switches[1].DialPeerWithAddress(switches[0].NetAddr())
	require.NoError(t, err)

	// sleep up to a second while waiting for the peer to send us a message.
	// this isn't perfect since it's possible the peer sends us a msg and we FlushStop
	// before this loop catches it. but non-deterministically it works pretty well.
	for i := 0; i < 1000; i++ {
		v := reactor.lastReceivedRequests.Get(peerID)
		if v != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// by now the FlushStop should have happened. Try stopping the peer.
	// it should be safe to do this.
	peers := switches[0].Peers().Copy()
	for _, peer := range peers {
		err := peer.Stop()
		require.NoError(t, err)
	}

	// stop the switches
	for _, s := range switches {
		err := s.Stop()
		require.NoError(t, err)
	}
}

func TestPEXReactorDoesNotAddPrivatePeersToAddrBook(t *testing.T) {
	peer := p2p.CreateRandomPeer(false)

	pexR, book := createReactor(&ReactorConfig{})
	book.AddPrivateIDs([]string{peer.NodeInfo().ID()})
	defer teardownReactor(book)

	// we have to send a request to receive responses
	pexR.RequestAddrs(peer)

	size := book.Size()
	msg := &tmp2p.PexAddrs{Addrs: []tmp2p.NetAddress{peer.SocketAddr().ToProto()}}
	pexR.Receive(p2p.Envelope{
		ChannelID: PexChannel,
		Src:       peer,
		Message:   msg,
	})
	assert.Equal(t, size, book.Size())

	pexR.AddPeer(peer)
	assert.Equal(t, size, book.Size())
}

func TestPEXReactorDialPeer(t *testing.T) {
	pexR, book := createReactor(&ReactorConfig{})
	defer teardownReactor(book)

	sw := createSwitchAndAddReactors(pexR)
	sw.SetAddrBook(book)

	peer := mock.NewPeer(nil)
	addr := peer.SocketAddr()

	assert.Equal(t, 0, pexR.AttemptsToDial(addr))

	// 1st unsuccessful attempt
	err := pexR.dialPeer(addr)
	require.Error(t, err)

	assert.Equal(t, 1, pexR.AttemptsToDial(addr))

	// 2nd unsuccessful attempt
	err = pexR.dialPeer(addr)
	require.Error(t, err)

	// must be skipped because it is too early
	assert.Equal(t, 1, pexR.AttemptsToDial(addr))

	if !testing.Short() {
		time.Sleep(3 * time.Second)

		// 3rd attempt
		err = pexR.dialPeer(addr)
		require.Error(t, err)

		assert.Equal(t, 2, pexR.AttemptsToDial(addr))
	}
}

func assertPeersWithTimeout(
	t *testing.T,
	switches []*p2p.Switch,
	timeout time.Duration,
	nPeers int,
) {
	t.Helper()
	checkPeriod := 10 * time.Millisecond

	var (
		ticker    = time.NewTicker(checkPeriod)
		timeoutCh = time.After(timeout)
	)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// check peers are connected
			allGood := true
			for _, s := range switches {
				outbound, inbound, _ := s.NumPeers()
				if outbound+inbound < nPeers {
					allGood = false
					break
				}
			}
			if allGood {
				return
			}
		case <-timeoutCh:
			numPeersStr := ""
			for i, s := range switches {
				outbound, inbound, _ := s.NumPeers()
				numPeersStr += fmt.Sprintf("%d => {outbound: %d, inbound: %d}, ", i, outbound, inbound)
			}
			t.Errorf(
				"expected all switches to be connected to at least %d peer(s) (switches: %s)",
				nPeers, numPeersStr,
			)
			return
		}
	}
}

// Creates a peer with the provided config.
func testCreatePeerWithConfig(dir string, id int, config *ReactorConfig) *p2p.Switch {
	if config.EnsurePeersPeriod == 0 {
		config.EnsurePeersPeriod = 250 * time.Millisecond
	}

	return p2p.MakeSwitch(
		cfg,
		id,
		func(_ int, sw *p2p.Switch) *p2p.Switch {
			logger := log.TestingLogger().With("pex", id)

			book := NewAddrBook(filepath.Join(dir, fmt.Sprintf("addrbook%d.json", id)), false)
			book.SetLogger(logger)
			sw.SetAddrBook(book)

			r := NewReactor(book, config)
			r.SetLogger(logger)

			sw.SetLogger(logger)
			sw.AddReactor("PEX", r)

			return sw
		},
	)
}

// Creates a peer with the default config.
func testCreateDefaultPeer(dir string, id int) *p2p.Switch {
	return testCreatePeerWithConfig(dir, id, &ReactorConfig{})
}

// Creates a seed which knows about the provided addresses / source address pairs.
// Starting and stopping the seed is left to the caller.
func testCreateSeed(dir string, id int, knownAddrs, srcAddrs []*na.NetAddr) *p2p.Switch {
	seed := p2p.MakeSwitch(
		cfg,
		id,
		func(_ int, sw *p2p.Switch) *p2p.Switch {
			logger := log.TestingLogger().With("seed", id)

			book := NewAddrBook(filepath.Join(dir, "addrbookSeed.json"), false)
			book.SetLogger(logger)
			for j := 0; j < len(knownAddrs); j++ {
				book.AddAddress(knownAddrs[j], srcAddrs[j]) //nolint:errcheck // ignore for tests
				book.MarkGood(knownAddrs[j].ID)
			}
			sw.SetAddrBook(book)

			r := NewReactor(book, &ReactorConfig{
				// Makes the tests fail ¯\_(ツ)_/¯
				// SeedMode: true,
				EnsurePeersPeriod: 250 * time.Millisecond,
			})
			r.SetLogger(logger)

			sw.SetLogger(logger)
			sw.AddReactor("PEX", r)

			return sw
		},
	)
	return seed
}

// Creates a peer which knows about the provided seed.
// Starting and stopping the peer is left to the caller.
func testCreatePeerWithSeed(dir string, id int, seed *p2p.Switch) *p2p.Switch {
	conf := &ReactorConfig{
		Seeds: []string{seed.NetAddr().String()},
	}
	return testCreatePeerWithConfig(dir, id, conf)
}

func createReactor(conf *ReactorConfig) (r *Reactor, book AddrBook) {
	// directory to store address book
	dir, err := os.MkdirTemp("", "pex_reactor")
	if err != nil {
		panic(err)
	}
	book = NewAddrBook(filepath.Join(dir, "addrbook.json"), true)
	book.SetLogger(log.TestingLogger())

	r = NewReactor(book, conf)
	r.SetLogger(log.TestingLogger())
	return r, book
}

func teardownReactor(book AddrBook) {
	// FIXME Shouldn't rely on .(*addrBook) assertion
	err := os.RemoveAll(filepath.Dir(book.(*addrBook).FilePath()))
	if err != nil {
		panic(err)
	}
}

func createSwitchAndAddReactors(reactors ...p2p.Reactor) *p2p.Switch {
	sw := p2p.MakeSwitch(cfg, 0, func(_ int, sw *p2p.Switch) *p2p.Switch { return sw })
	sw.SetLogger(log.TestingLogger())
	for _, r := range reactors {
		sw.AddReactor(r.String(), r)
		r.SetSwitch(sw)
	}
	return sw
}

func TestPexVectors(t *testing.T) {
	addr := tmp2p.NetAddress{
		ID:   "1",
		IP:   "127.0.0.1",
		Port: 9090,
	}

	testCases := []struct {
		testName string
		msg      proto.Message
		expBytes string
	}{
		{"PexRequest", &tmp2p.PexRequest{}, "0a00"},
		{"PexAddrs", &tmp2p.PexAddrs{Addrs: []tmp2p.NetAddress{addr}}, "12130a110a013112093132372e302e302e31188247"},
	}

	for _, tc := range testCases {
		w := tc.msg.(types.Wrapper).Wrap()
		bz, err := proto.Marshal(w)
		require.NoError(t, err)

		require.Equal(t, tc.expBytes, hex.EncodeToString(bz), tc.testName)
	}
}
