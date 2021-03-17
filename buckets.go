package buckets

// @todo: Validate all thread IDs
// @todo: Validate all identities
// @todo: Clean up error messages
// @todo: Enforce fast-forward-only in SetPath and MovePath, PushPathAccessRoles

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	c "github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	iface "github.com/ipfs/interface-go-ipfs-core"
	"github.com/ipfs/interface-go-ipfs-core/path"
	"github.com/textileio/go-buckets/collection"
	"github.com/textileio/go-buckets/dag"
	"github.com/textileio/go-buckets/dns"
	"github.com/textileio/go-buckets/ipns"
	dbc "github.com/textileio/go-threads/api/client"
	"github.com/textileio/go-threads/core/did"
	core "github.com/textileio/go-threads/core/thread"
	"github.com/textileio/go-threads/db"
	nc "github.com/textileio/go-threads/net/api/client"
	nutil "github.com/textileio/go-threads/net/util"
)

var (
	log = logging.Logger("buckets")

	// GatewayURL is used to construct externally facing bucket links.
	GatewayURL string

	// ThreadsGatewayURL is used to construct externally facing bucket links.
	ThreadsGatewayURL string

	// WWWDomain can be set to specify the domain to use for bucket website hosting, e.g.,
	// if this is set to mydomain.com, buckets can be rendered as a website at the following URL:
	//   https://<bucket_key>.mydomain.com
	WWWDomain string

	// ErrNonFastForward is returned when an update in non-fast-forward.
	ErrNonFastForward = errors.New("update is non-fast-forward")

	movePathRegexp = regexp.MustCompile("/ipfs/([^/]+)/")
)

// Bucket adds thread ID to collection.Bucket.
type Bucket struct {
	Thread core.ID `json:"thread"`
	collection.Bucket
}

// PathItem describes a file or directory in a bucket.
type PathItem struct {
	Cid        string              `json:"cid"`
	Name       string              `json:"name"`
	Path       string              `json:"path"`
	Size       int64               `json:"size"`
	IsDir      bool                `json:"is_dir"`
	Items      []PathItem          `json:"items"`
	ItemsCount int32               `json:"items_count"`
	Metadata   collection.Metadata `json:"metadata"`
}

// Links wraps links for resolving a bucket with various protocols.
type Links struct {
	// URL is the thread URL, which maps to a ThreadDB collection instance.
	URL string `json:"url"`
	// WWW is the URL at which the bucket will be rendered as a website (requires remote DNS configuration).
	WWW string `json:"www"`
	// IPNS is the bucket IPNS address.
	IPNS string `json:"ipns"`
}

// Seed describes a bucket seed file.
type Seed struct {
	Cid  c.Cid
	Data []byte
}

// Buckets is an object storage library built on Threads, IPFS, and IPNS.
type Buckets struct {
	net *nc.Client
	db  *dbc.Client
	c   *collection.Buckets

	ipfs iface.CoreAPI
	ipns *ipns.Manager
	dns  *dns.Manager

	locks *nutil.SemaphorePool
}

var _ nutil.SemaphoreKey = (*lock)(nil)

type lock string

func (l lock) Key() string {
	return string(l)
}

// NewBuckets returns a new buckets library.
func NewBuckets(
	net *nc.Client,
	db *dbc.Client,
	ipfs iface.CoreAPI,
	ipns *ipns.Manager,
	dns *dns.Manager,
) (*Buckets, error) {
	bc, err := collection.NewBuckets(db)
	if err != nil {
		return nil, fmt.Errorf("getting buckets collection: %v", err)
	}
	return &Buckets{
		net:   net,
		db:    db,
		c:     bc,
		ipfs:  ipfs,
		ipns:  ipns,
		dns:   dns,
		locks: nutil.NewSemaphorePool(1),
	}, nil
}

// Close it down.
func (b *Buckets) Close() error {
	b.locks.Stop()
	return nil
}

func (b *Buckets) Net() *nc.Client {
	return b.net
}

func (b *Buckets) DB() *dbc.Client {
	return b.db
}

func (b *Buckets) Get(ctx context.Context, thread core.ID, key string, identity did.Token) (*Bucket, error) {
	instance, err := b.c.GetSafe(ctx, thread, key, collection.WithIdentity(identity))
	if err != nil {
		return nil, err
	}
	log.Debugf("got %s", key)
	return instanceToBucket(thread, instance), nil
}

func (b *Buckets) GetLinks(
	ctx context.Context,
	thread core.ID,
	key, pth string,
	identity did.Token,
) (links Links, err error) {
	instance, err := b.c.GetSafe(ctx, thread, key, collection.WithIdentity(identity))
	if err != nil {
		return links, err
	}
	log.Debugf("got %s links", key)
	return b.GetLinksForBucket(ctx, instanceToBucket(thread, instance), pth, identity)
}

func (b *Buckets) GetLinksForBucket(
	ctx context.Context,
	bucket *Bucket,
	pth string,
	identity did.Token,
) (links Links, err error) {
	links.URL = fmt.Sprintf("%s/thread/%s/%s/%s", ThreadsGatewayURL, bucket.Thread, collection.Name, bucket.Key)
	if len(WWWDomain) != 0 {
		parts := strings.Split(GatewayURL, "://")
		if len(parts) < 2 {
			return links, fmt.Errorf("failed to parse gateway URL: %s", GatewayURL)
		}
		links.WWW = fmt.Sprintf("%s://%s.%s", parts[0], bucket.Key, WWWDomain)
	}
	links.IPNS = fmt.Sprintf("%s/ipns/%s", GatewayURL, bucket.Key)

	pth = trimSlash(pth)
	if _, _, ok := bucket.GetMetadataForPath(pth, false); !ok {
		return links, fmt.Errorf("could not resolve path: %s", pth)
	}
	if len(pth) != 0 {
		npth, err := getBucketPath(&bucket.Bucket, pth)
		if err != nil {
			return links, err
		}
		linkKey := bucket.GetLinkEncryptionKey()
		if _, err := dag.GetNodeAtPath(ctx, b.ipfs, npth, linkKey); err != nil {
			return links, err
		}
		pth = "/" + pth
		links.URL += pth
		if len(links.WWW) != 0 {
			links.WWW += pth
		}
		links.IPNS += pth
	}

	query := "?token=" + string(identity)
	links.URL += query
	if len(links.WWW) != 0 {
		links.WWW += query
	}
	links.IPNS += query

	return links, nil
}

func (b *Buckets) List(ctx context.Context, thread core.ID, identity did.Token) ([]Bucket, error) {
	list, err := b.c.List(ctx, thread, &db.Query{}, &collection.Bucket{}, collection.WithIdentity(identity))
	if err != nil {
		return nil, fmt.Errorf("listing buckets: %v", err)
	}
	instances := list.([]*collection.Bucket)
	bucks := make([]Bucket, len(instances))
	for i, in := range instances {
		bucket := instanceToBucket(thread, in)
		bucks[i] = *bucket
	}

	log.Debugf("listed all in %s", thread)
	return bucks, nil
}

func (b *Buckets) Remove(ctx context.Context, thread core.ID, key string, identity did.Token) (int64, error) {
	lk := b.locks.Get(lock(key))
	lk.Acquire()
	defer lk.Release()

	instance, err := b.c.GetSafe(ctx, thread, key, collection.WithIdentity(identity))
	if err != nil {
		return 0, err
	}
	if err := b.c.Delete(ctx, thread, key, collection.WithIdentity(identity)); err != nil {
		return 0, fmt.Errorf("deleting bucket: %v", err)
	}

	buckPath, err := dag.NewResolvedPath(instance.Path)
	if err != nil {
		return 0, fmt.Errorf("resolving path: %v", err)
	}
	linkKey := instance.GetLinkEncryptionKey()
	if linkKey != nil {
		ctx, err = dag.UnpinNodeAndBranch(ctx, b.ipfs, buckPath, linkKey)
		if err != nil {
			return 0, err
		}
	} else {
		ctx, err = dag.UnpinPath(ctx, b.ipfs, buckPath)
		if err != nil {
			return 0, err
		}
	}
	if err := b.ipns.RemoveKey(ctx, key); err != nil {
		return 0, err
	}

	log.Debugf("removed %s", key)
	return dag.GetPinnedBytes(ctx), nil
}

func (b *Buckets) saveAndPublish(
	ctx context.Context,
	thread core.ID,
	instance *collection.Bucket,
	identity did.Token,
) error {
	if err := b.c.Save(ctx, thread, instance, collection.WithIdentity(identity)); err != nil {
		return fmt.Errorf("saving bucket: %v", err)
	}
	go b.ipns.Publish(path.New(instance.Path), instance.Key)
	return nil
}

func instanceToBucket(thread core.ID, instance *collection.Bucket) *Bucket {
	return &Bucket{
		Thread: thread,
		Bucket: *instance,
	}
}
