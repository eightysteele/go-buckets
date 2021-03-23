package gateway

import (
	"context"
	"crypto/rand"
	"fmt"
	"io/ioutil"
	"os"
	"sync/atomic"
	"testing"
	"time"

	ipfsfiles "github.com/ipfs/go-ipfs-files"
	httpapi "github.com/ipfs/go-ipfs-http-client"
	logging "github.com/ipfs/go-log/v2"
	psc "github.com/ipfs/go-pinning-service-http-client"
	"github.com/ipfs/interface-go-ipfs-core/options"
	"github.com/ipfs/interface-go-ipfs-core/path"
	"github.com/libp2p/go-libp2p-core/crypto"
	maddr "github.com/multiformats/go-multiaddr"
	"github.com/phayes/freeport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/textileio/go-buckets"
	"github.com/textileio/go-buckets/api/apitest"
	"github.com/textileio/go-buckets/api/common"
	"github.com/textileio/go-buckets/cmd"
	"github.com/textileio/go-buckets/ipns"
	"github.com/textileio/go-buckets/pinning"
	"github.com/textileio/go-buckets/pinning/queue"
	dbc "github.com/textileio/go-threads/api/client"
	"github.com/textileio/go-threads/core/did"
	"github.com/textileio/go-threads/core/thread"
	tdb "github.com/textileio/go-threads/db"
	nc "github.com/textileio/go-threads/net/api/client"
	"github.com/textileio/go-threads/util"
	"golang.org/x/sync/errgroup"
)

var (
	origins   []maddr.Multiaddr
	statusAll = []psc.Status{psc.StatusQueued, psc.StatusPinning, psc.StatusPinned, psc.StatusFailed}
)

func init() {
	if err := util.SetLogLevels(map[string]logging.LogLevel{
		"buckets":          logging.LevelDebug,
		"buckets/ps":       logging.LevelDebug,
		"buckets/ps-queue": logging.LevelDebug,
		"buckets/gateway":  logging.LevelDebug,
	}); err != nil {
		panic(err)
	}
}

func TestMain(m *testing.M) {
	cleanup := func() {}
	if os.Getenv("SKIP_SERVICES") != "true" {
		cleanup = apitest.StartServices()
	}

	var err error
	origins, err = getOrigins()
	if err != nil {
		log.Fatalf("failed to get ipfs node origins: %v", err)
	}

	exitVal := m.Run()
	cleanup()
	os.Exit(exitVal)
}

func Test_ListPins(t *testing.T) {
	queue.MaxConcurrency = 5 // Reduce concurrency to test overloading workers
	pinning.PinTimeout = time.Second * 10
	gw := newGateway(t)

	numBatches := 10
	batchSize := 20
	total := numBatches * batchSize

	files := make([]path.Resolved, total)
	for i := 0; i < total; i++ {
		files[i] = createIpfsFile(t, i%(batchSize/2) == 0) // Two-per batch should fail (blocks unavailable)
		log.Debugf("created file %d", i)
	}

	var done int32
	clients := make([]*psc.Client, numBatches)
	for b := 0; b < numBatches; b++ {
		c := newClient(t, gw) // New client and bucket
		clients[b] = c
		go func(c *psc.Client, b int) {
			eg, gctx := errgroup.WithContext(context.Background())
			for i := 0; i < batchSize; i++ {
				i := i
				j := i + (b * batchSize)
				f := files[j]
				eg.Go(func() error {
					if gctx.Err() != nil {
						return nil
					}
					_, err := c.Add(gctx, f.Cid(), psc.PinOpts.WithOrigins(origins...))
					atomic.AddInt32(&done, 1)
					return err
				})
			}
			err := eg.Wait()
			require.NoError(t, err)
		}(c, b)
	}

	time.Sleep(time.Second * 5) // Allow time for requests to be added

	// Test pagination
	for _, c := range clients {
		res, err := c.LsSync(context.Background(), psc.PinOpts.FilterStatus(statusAll...))
		require.NoError(t, err)
		assert.Len(t, res, 10)
	}

	// Wait for all to complete
	assert.Eventually(t, func() bool {
		// Check if all requests have been sent
		if atomic.LoadInt32(&done) != int32(total) {
			return false
		}
		// Check if all request have completed
		for _, c := range clients {
			res, err := c.LsSync(context.Background(),
				psc.PinOpts.FilterStatus(psc.StatusQueued, psc.StatusPinning),
				psc.PinOpts.Limit(batchSize),
			)
			require.NoError(t, err)
			if len(res) != 0 {
				return false
			}
		}
		return true
	}, time.Minute*10, time.Second*5)

	// Test expected counts
	for _, c := range clients {
		res, err := c.LsSync(context.Background(),
			psc.PinOpts.FilterStatus(psc.StatusPinned),
			psc.PinOpts.Limit(batchSize),
		)
		require.NoError(t, err)
		assert.Len(t, res, batchSize-2)

		res, err = c.LsSync(context.Background(),
			psc.PinOpts.FilterStatus(psc.StatusFailed),
			psc.PinOpts.Limit(batchSize),
		)
		require.NoError(t, err)
		assert.Len(t, res, 2)
	}
}

func Test_AddPin(t *testing.T) {
	queue.MaxConcurrency = 100
	pinning.PinTimeout = time.Second * 5
	gw := newGateway(t)
	c := newClient(t, gw)

	t.Run("add unavailable pin should fail", func(t *testing.T) {
		t.Parallel()
		folder := createIpfsFolder(t, true)
		res, err := c.Add(context.Background(), folder.Cid(), psc.PinOpts.WithOrigins(origins...))
		require.NoError(t, err)
		assert.NotEmpty(t, res.GetRequestId())
		assert.NotEmpty(t, res.GetCreated())
		assert.Equal(t, psc.StatusQueued, res.GetStatus())

		time.Sleep(time.Second * 10) // Allow for the pin to fail

		res, err = c.GetStatusByID(context.Background(), res.GetRequestId())
		require.NoError(t, err)
		assert.Equal(t, psc.StatusFailed, res.GetStatus())
	})

	t.Run("add available pin should succeed", func(t *testing.T) {
		t.Parallel()
		folder := createIpfsFolder(t, false)
		res, err := c.Add(context.Background(), folder.Cid(), psc.PinOpts.WithOrigins(origins...))
		require.NoError(t, err)
		assert.NotEmpty(t, res.GetRequestId())
		assert.NotEmpty(t, res.GetCreated())
		assert.Equal(t, psc.StatusQueued, res.GetStatus())

		time.Sleep(time.Second * 10) // Allow for the pin to succeed

		res, err = c.GetStatusByID(context.Background(), res.GetRequestId())
		require.NoError(t, err)
		assert.Equal(t, psc.StatusPinned, res.GetStatus())
		assert.True(t, res.GetPin().GetCid().Equals(folder.Cid()))
	})
}

func newGateway(t *testing.T) *Gateway {
	threadsAddr := apitest.GetThreadsApiAddr()
	net, err := nc.NewClient(threadsAddr, common.GetClientRPCOpts(threadsAddr)...)
	require.NoError(t, err)

	db, err := dbc.NewClient(threadsAddr, common.GetClientRPCOpts(threadsAddr)...)
	require.NoError(t, err)
	ipfs, err := httpapi.NewApi(apitest.GetIPFSApiMultiAddr())
	require.NoError(t, err)
	ipnsms := tdb.NewTxMapDatastore()
	ipnsm, err := ipns.NewManager(ipnsms, ipfs)
	require.NoError(t, err)
	lib, err := buckets.NewBuckets(net, db, ipfs, ipnsm, nil)
	require.NoError(t, err)

	dir, err := ioutil.TempDir("", "")
	require.NoError(t, err)
	pss, err := util.NewBadgerDatastore(dir, "pinq", false)
	require.NoError(t, err)
	ps := pinning.NewService(lib, pss)

	listenPort, err := freeport.GetFreePort()
	require.NoError(t, err)
	addr := fmt.Sprintf("127.0.0.1:%d", listenPort)
	baseUrl := fmt.Sprintf("http://127.0.0.1:%d", listenPort)
	gw, err := NewGateway(lib, ipfs, ipnsm, ps, Config{
		Addr: addr,
		URL:  baseUrl,
	})
	cmd.ErrCheck(err)
	gw.Start()

	t.Cleanup(func() {
		require.NoError(t, gw.Close())
		require.NoError(t, ps.Close())
		require.NoError(t, pss.Close())
		require.NoError(t, lib.Close())
		require.NoError(t, ipnsm.Close())
		require.NoError(t, ipnsms.Close())
		require.NoError(t, db.Close())
		require.NoError(t, net.Close())
	})
	return gw
}

func newClient(t *testing.T, gw *Gateway) *psc.Client {
	token := newIdentityToken(t)
	buck, _, _, err := gw.lib.Create(context.Background(), token)
	require.NoError(t, err)
	url := fmt.Sprintf("%s/bps/%s", gw.url, buck.Key)
	return psc.NewClient(url, string(token))
}

func newIdentityToken(t *testing.T) did.Token {
	sk, _, err := crypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	id := thread.NewLibp2pIdentity(sk)
	token, err := id.Token("did:key:foo", time.Hour)
	require.NoError(t, err)
	return token
}

func createIpfsFile(t *testing.T, hashOnly bool) (pth path.Resolved) {
	ipfs, err := httpapi.NewApi(apitest.GetIPFSApiMultiAddr())
	require.NoError(t, err)
	pth, err = ipfs.Unixfs().Add(
		context.Background(),
		ipfsfiles.NewMapDirectory(map[string]ipfsfiles.Node{
			"file.txt": ipfsfiles.NewBytesFile(util.GenerateRandomBytes(512)),
		}),
		options.Unixfs.HashOnly(hashOnly),
	)
	require.NoError(t, err)
	return pth
}

func createIpfsFolder(t *testing.T, hashOnly bool) (pth path.Resolved) {
	ipfs, err := httpapi.NewApi(apitest.GetIPFSApiMultiAddr())
	require.NoError(t, err)
	pth, err = ipfs.Unixfs().Add(
		context.Background(),
		ipfsfiles.NewMapDirectory(map[string]ipfsfiles.Node{
			"file1.txt": ipfsfiles.NewBytesFile(util.GenerateRandomBytes(1024)),
			"folder1": ipfsfiles.NewMapDirectory(map[string]ipfsfiles.Node{
				"file2.txt": ipfsfiles.NewBytesFile(util.GenerateRandomBytes(512)),
			}),
		}),
		options.Unixfs.HashOnly(hashOnly),
	)
	require.NoError(t, err)
	return pth
}

func getOrigins() ([]maddr.Multiaddr, error) {
	ipfs, err := httpapi.NewApi(apitest.GetIPFSApiMultiAddr())
	if err != nil {
		return nil, err
	}
	key, err := ipfs.Key().Self(context.Background())
	if err != nil {
		return nil, err
	}
	paddr, err := maddr.NewMultiaddr("/p2p/" + key.ID().String())
	if err != nil {
		return nil, err
	}
	addrs, err := ipfs.Swarm().LocalAddrs(context.Background())
	if err != nil {
		return nil, err
	}
	paddrs := make([]maddr.Multiaddr, len(addrs))
	for i, a := range addrs {
		paddrs[i] = a.Encapsulate(paddr)
	}
	return paddrs, nil
}
