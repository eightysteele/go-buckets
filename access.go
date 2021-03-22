package buckets

import (
	"context"
	"fmt"
	"time"

	ipld "github.com/ipfs/go-ipld-format"
	"github.com/ipfs/interface-go-ipfs-core/path"
	"github.com/textileio/dcrypto"
	"github.com/textileio/go-buckets/collection"
	"github.com/textileio/go-buckets/dag"
	"github.com/textileio/go-threads/core/did"
	core "github.com/textileio/go-threads/core/thread"
)

func (b *Buckets) PushPathAccessRoles(
	ctx context.Context,
	thread core.ID,
	key string,
	identity did.Token,
	root path.Resolved,
	pth string,
	roles map[did.DID]collection.Role,
) (int64, *Bucket, error) {
	txn := b.NewTxn(thread, key, identity)
	defer txn.Close()
	return txn.PushPathAccessRoles(ctx, root, pth, roles)
}

func (t *Txn) PushPathAccessRoles(
	ctx context.Context,
	root path.Resolved,
	pth string,
	roles map[did.DID]collection.Role,
) (int64, *Bucket, error) {
	pth, err := parsePath(pth)
	if err != nil {
		return 0, nil, err
	}

	instance, bpth, err := t.b.getBucketAndPath(ctx, t.thread, t.key, t.identity, pth)
	if err != nil {
		return 0, nil, err
	}
	if root != nil && root.String() != instance.Path {
		return 0, nil, ErrNonFastForward
	}

	linkKey := instance.GetLinkEncryptionKey()
	pathNode, err := dag.GetNodeAtPath(ctx, t.b.ipfs, bpth, linkKey)
	if err != nil {
		return 0, nil, err
	}

	md, mdPath, ok := instance.GetMetadataForPath(pth, false)
	if !ok {
		return 0, nil, fmt.Errorf("could not resolve path: %s", pth)
	}
	var target collection.Metadata
	if mdPath != pth { // If the metadata is inherited from a parent, create a new entry
		target = collection.Metadata{
			Roles: make(map[did.DID]collection.Role),
		}
	} else {
		target = md
	}
	var currentFileKeys map[string][]byte
	if instance.IsPrivate() {
		currentFileKeys, err = instance.GetFileEncryptionKeysForPrefix(pth)
		if err != nil {
			return 0, nil, err
		}
		newFileKey, err := dcrypto.NewKey()
		if err != nil {
			return 0, nil, err
		}
		target.SetFileEncryptionKey(newFileKey) // Create or refresh the file key
	}

	var changed bool
	for k, r := range roles {
		if x, ok := target.Roles[k]; ok && x == r {
			continue
		}
		if r > collection.NoneRole {
			target.Roles[k] = r
		} else {
			delete(target.Roles, k)
		}
		changed = true
	}
	if changed {
		instance.UpdatedAt = time.Now().UnixNano()
		target.UpdatedAt = instance.UpdatedAt
		instance.Metadata[pth] = target
		if instance.IsPrivate() {
			if err := instance.RotateFileEncryptionKeysForPrefix(pth); err != nil {
				return 0, nil, err
			}
		}
		if err := t.b.c.Verify(ctx, t.thread, instance, collection.WithIdentity(t.identity)); err != nil {
			return 0, nil, err
		}

		if instance.IsPrivate() {
			newFileKeys, err := instance.GetFileEncryptionKeysForPrefix(pth)
			if err != nil {
				return 0, nil, err
			}
			nmap, err := dag.EncryptDag(
				ctx,
				t.b.ipfs,
				pathNode,
				pth,
				linkKey,
				currentFileKeys,
				newFileKeys,
				nil,
				nil,
			)
			if err != nil {
				return 0, nil, err
			}
			nodes := make([]ipld.Node, len(nmap))
			i := 0
			for _, tn := range nmap {
				nodes[i] = tn.Node
				i++
			}
			pn := nmap[pathNode.Cid()].Node
			var dirPath path.Resolved
			ctx, dirPath, err = dag.InsertNodeAtPath(ctx, t.b.ipfs, pn, path.Join(path.New(instance.Path), pth), linkKey)
			if err != nil {
				return 0, nil, err
			}
			ctx, err = dag.AddAndPinNodes(ctx, t.b.ipfs, nodes)
			if err != nil {
				return 0, nil, err
			}
			instance.Path = dirPath.String()
		}

		if err := t.b.c.Save(ctx, t.thread, instance, collection.WithIdentity(t.identity)); err != nil {
			return 0, nil, err
		}
	}

	log.Debugf("pushed access roles for %s in %s", pth, t.key)
	return dag.GetPinnedBytes(ctx), instanceToBucket(t.thread, instance), nil
}

func (b *Buckets) PullPathAccessRoles(
	ctx context.Context,
	thread core.ID,
	key string,
	identity did.Token,
	pth string,
) (map[did.DID]collection.Role, error) {
	pth, err := parsePath(pth)
	if err != nil {
		return nil, err
	}
	instance, err := b.c.GetSafe(ctx, thread, key, collection.WithIdentity(identity))
	if err != nil {
		return nil, err
	}
	md, _, ok := instance.GetMetadataForPath(pth, false)
	if !ok {
		return nil, fmt.Errorf("could not resolve path: %s", pth)
	}

	log.Debugf("pulled access roles for %s in %s", pth, key)
	return md.Roles, nil
}
