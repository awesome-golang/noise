package skademlia

import (
	"bytes"
	"fmt"
	"github.com/perlin-network/noise"
	"github.com/phf/go-queue/queue"
	"github.com/pkg/errors"
	"golang.org/x/crypto/blake2b"
	"net"
	"sort"
	"sync"
	"time"
)

const (
	DefaultPrefixDiffLen = 128
	DefaultPrefixDiffMin = 32

	DefaultC1 = 16
	DefaultC2 = 16

	SignalHandshakeComplete = "skademlia.handshake"
)

type Protocol struct {
	table *Table
	keys  *Keypair

	prefixDiffLen int
	prefixDiffMin int

	c1, c2 int

	handshakeTimeout time.Duration
	findNodeTimeout  time.Duration

	peers     map[[blake2b.Size256]byte]*noise.Peer
	peersLock sync.Mutex
}

func New(keys *Keypair, externalAddress string) *Protocol {
	return &Protocol{
		table: NewTable(keys.ID(externalAddress)),
		keys:  keys,

		prefixDiffLen: DefaultPrefixDiffLen,
		prefixDiffMin: DefaultPrefixDiffMin,

		c1: DefaultC1,
		c2: DefaultC2,

		handshakeTimeout: 3 * time.Second,
		findNodeTimeout:  3 * time.Second,

		peers: make(map[[blake2b.Size256]byte]*noise.Peer),
	}
}

func (b *Protocol) WithC1(c1 int) *Protocol {
	b.c1 = c1
	return b
}

func (b *Protocol) WithC2(c2 int) *Protocol {
	b.c2 = c2
	return b
}

func (b *Protocol) WithPrefixDiffLen(prefixDiffLen int) *Protocol {
	b.prefixDiffLen = prefixDiffLen
	return b
}

func (b *Protocol) WithPrefixDiffMin(prefixDiffMin int) *Protocol {
	b.prefixDiffMin = prefixDiffMin
	return b
}

func (b *Protocol) WithHandshakeTimeout(handshakeTimeout time.Duration) *Protocol {
	b.handshakeTimeout = handshakeTimeout
	return b
}

func (b *Protocol) Peers(node *noise.Node) (peers []*noise.Peer) {
	ids := b.table.FindClosest(b.table.self, b.table.bucketSize)

	for _, id := range ids {
		if peer := b.PeerByID(node, id); peer != nil {
			peers = append(peers, peer)
		}
	}

	return
}

func (b *Protocol) PeerByID(node *noise.Node, id *ID) *noise.Peer {
	b.peersLock.Lock()
	peer, recorded := b.peers[id.checksum]
	b.peersLock.Unlock()

	if recorded {
		return peer
	}

	peer = node.PeerByAddr(id.address)

	if peer != nil {
		return peer
	}

	peer, err := node.Dial(id.address)

	if err != nil {
		b.evict(id)
		return nil
	}

	return peer
}

func wrap(f func() error) {
	_ = f()
}

func (b *Protocol) Ping(ctx noise.Context) (*ID, error) {
	mux := ctx.Peer().Mux()
	defer wrap(mux.Close)

	if err := mux.Send(0x03, nil); err != nil {
		return nil, errors.Wrap(err, "failed to send ping")
	}

	var buf []byte

	select {
	case <-ctx.Done():
		return nil, noise.ErrDisconnect
	case <-time.After(b.handshakeTimeout):
		return nil, errors.Wrap(noise.ErrTimeout, "timed out receiving pong")
	case ctx := <-mux.Recv(0x03):
		buf = ctx.Bytes()
	}

	id, err := UnmarshalID(bytes.NewReader(buf))

	if err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal pong")
	}

	if err := verifyPuzzle(id.checksum, id.nonce, b.c1, b.c2); err != nil {
		return nil, errors.Wrap(err, "peer connected with invalid id")
	}

	if prefixDiff(b.table.self.checksum[:], id.checksum[:], b.prefixDiffLen) < b.prefixDiffMin {
		return nil, errors.New("peer id is too similar to ours")
	}

	return &id, err
}

func (b *Protocol) Lookup(ctx noise.Context, target *ID) (IDs, error) {
	mux := ctx.Peer().Mux()
	defer wrap(mux.Close)

	if err := mux.Send(0x04, target.Marshal()); err != nil {
		return nil, errors.Wrap(err, "failed to send find node request")
	}

	var buf []byte

	select {
	case <-ctx.Done():
		return nil, noise.ErrDisconnect
	case <-time.After(b.handshakeTimeout):
		return nil, errors.Wrap(noise.ErrTimeout, "timed out receiving finde node response")
	case ctx := <-mux.Recv(0x04):
		buf = ctx.Bytes()
	}

	return UnmarshalIDs(bytes.NewReader(buf))
}

func (b *Protocol) Handshake(ctx noise.Context) (*ID, error) {
	signal := ctx.Peer().RegisterSignal(SignalHandshakeComplete)
	defer signal()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case wire := <-ctx.Peer().Recv(0x03): // PING
				if err := wire.Send(0x03, b.table.self.Marshal()); err != nil {
					ctx.Peer().Disconnect(errors.Wrap(err, "failed to send ping"))
				}
			case wire := <-ctx.Peer().Recv(0x04): // LOOKUP REQUEST
				target, err := UnmarshalID(bytes.NewReader(wire.Bytes()))

				if err != nil {
					ctx.Peer().Disconnect(errors.Wrap(err, "sent invalid lookup request"))
				}

				if err := wire.Send(0x04, b.table.FindClosest(&target, b.table.bucketSize).Marshal()); err != nil {
					ctx.Peer().Disconnect(errors.Wrap(err, "failed tpo send lookup response"))
				}
			}
		}
	}()

	/*
		From here on out:

		1. Check if our current connection has the same address recorded in the ID.

		3. If not, establish a connection to the address recorded in the ID and
			see if its reachable (in other words, ping the address).
		4. If not reachable, disconnect the peer.

		5. Else, deregister the ping by disconnecting the connection created by the ping.

		6. Attempt to register the ID into our routing table.

		7. Should any errors occur on the connection lead to a disconnection,
			dissociate this connection from the ID.
	*/

	id, err := b.Ping(ctx)

	if err != nil {
		return nil, err
	}

	b.peersLock.Lock()
	_, existed := b.peers[id.checksum]
	b.peersLock.Unlock()

	if !existed && ctx.Peer().Addr().String() != id.address {
		reachable := b.PeerByID(ctx.Node(), id)

		if reachable == nil {
			return nil, noise.ErrTimeout
		}

		reachable.Disconnect(nil)
	}

	b.peersLock.Lock()
	b.peers[id.checksum] = ctx.Peer()
	b.peersLock.Unlock()

	if err := b.Update(id); err != nil {
		return nil, err
	}

	ctx.Peer().InterceptErrors(func(err error) {
		delete(b.peers, id.checksum)

		if err, ok := err.(net.Error); ok && err.Timeout() {
			b.evict(id)
			return
		}

		if errors.Cause(err) == noise.ErrTimeout {
			b.evict(id)
			return
		}
	})

	ctx.Peer().AfterRecv(func() {
		if err := b.Update(id); err != nil {
			ctx.Peer().Disconnect(err)
		}
	})

	fmt.Printf("Registered to S/Kademlia: %s\n", id)

	return id, nil
}

func (b *Protocol) Update(id *ID) error {
	for b.table.Update(id) == ErrBucketFull {
		bucket := b.table.buckets[getBucketID(b.table.self.checksum, id.checksum)]

		bucket.Lock()
		last := bucket.Back()
		bucket.Unlock()

		lastid := last.Value.(*ID)

		b.peersLock.Lock()
		lastp, exists := b.peers[lastid.checksum]
		b.peersLock.Unlock()

		if !exists {
			b.table.Delete(bucket, lastid)
			continue
		}

		pid, err := b.Ping(lastp.Ctx())

		if err != nil { // Failed to ping peer at back of bucket.
			lastp.Disconnect(errors.Wrap(noise.ErrTimeout, "failed to ping last peer in bucket"))
			continue
		}

		if pid.checksum != lastid.checksum || pid.nonce != lastid.nonce || pid.address != lastid.address { // Failed to authenticate peer at back of bucket.
			lastp.Disconnect(errors.Wrap(noise.ErrTimeout, "got invalid id pinging last peer in bucket"))
			continue
		}

		fmt.Printf("Routing table is full; evicting peer %s.\n", id)

		return errors.Wrap(noise.ErrDisconnect, "must reject peer: cannot evict any peers to make room for new peer")
	}

	return nil
}

func (b *Protocol) Bootstrap(node *noise.Node) (results []*ID) {
	return b.FindNode(node, b.table.self, b.table.bucketSize, 3, 8)
}

func (b *Protocol) FindNode(node *noise.Node, target *ID, k int, a int, d int) (results []*ID) {
	var mu sync.Mutex

	visited := map[[blake2b.Size256]byte]struct{}{
		b.table.self.checksum: {},
		target.checksum:       {},
	}

	lookups := make([]queue.Queue, d)

	for i, id := range b.table.FindClosest(target, k) {
		visited[id.checksum] = struct{}{}

		results = append(results, id)
		lookups[i%d].PushBack(id)
	}

	var wg sync.WaitGroup
	wg.Add(d)

	for _, lookup := range lookups { // Perform d parallel disjoint lookups.
		go func(lookup queue.Queue) {
			requests := make(chan *ID, a)
			responses := make(chan []*ID, a)

			for i := 0; i < a; i++ { // Perform α queries in parallel per disjoint lookup.
				go func() {
					for id := range requests {
						peer := b.PeerByID(node, id)

						if peer == nil {
							continue
						}

						ids, err := b.Lookup(peer.Ctx(), id)

						if err != nil {
							peer.Disconnect(err)
							continue
						}

						responses <- ids
					}
				}()
			}

			pending := 0

			for lookup.Len() > 0 {
				for lookup.Len() > 0 && len(requests) < cap(requests) {
					requests <- lookup.PopFront().(*ID)
					pending++
				}

				if pending > 0 {
					res := <-responses

					for _, id := range res {
						mu.Lock()
						if _, seen := visited[id.checksum]; !seen {
							visited[id.checksum] = struct{}{}

							results = append(results, id)
							lookup.PushBack(id)
						}
						mu.Unlock()
					}
				}
			}

			close(requests)

			wg.Done()
		}(lookup)
	}

	wg.Wait() // Wait until all d parallel disjoint lookups are complete.

	sort.Slice(results, func(i, j int) bool {
		return bytes.Compare(xor(results[i].checksum[:], target.checksum[:]), xor(results[j].checksum[:], target.checksum[:])) == -1
	})

	if len(results) > k {
		results = results[:k]
	}

	return
}

func (b *Protocol) evict(id *ID) {
	fmt.Printf("Peer %s could not be reached, and has been evicted.\n", id)

	bucket := b.table.buckets[getBucketID(b.table.self.checksum, id.checksum)]
	b.table.Delete(bucket, id)
}