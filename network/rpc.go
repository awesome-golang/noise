package network

import (
	"github.com/perlin-network/noise/peer"
	"github.com/perlin-network/noise/protobuf"
	"sync"
	"github.com/perlin-network/noise/log"
	"github.com/perlin-network/perlin-go/network/dht"
)

func BootstrapPeers(network *Network, target peer.ID, count int) (addresses []string, publicKeys [][]byte) {
	queue := []peer.ID{target}

	visited := make(map[string]struct{})
	visited[network.Keys.PublicKeyHex()] = struct{}{}
	visited[target.Hex()] = struct{}{}

	for len(queue) > 0 {
		var wait sync.WaitGroup
		wait.Add(len(queue))

		responses := make(chan *protobuf.LookupNodeResponse, len(queue))

		// Queue up all work into worker pools for contacting peers.
		for _, popped := range queue {
			go func(peerId peer.ID) {
				defer wait.Done()

				client, err := network.Client(peerId)
				if err != nil {
					return
				}

				protoId := protobuf.ID(peerId)

				request := &protobuf.LookupNodeRequest{
					Target: &protoId,
				}

				response, err := network.Request(client, request)

				if err != nil {
					log.Debug(response)
					return
				}

				if response, ok := response.(*protobuf.LookupNodeResponse); ok {
					log.Debug(response)
					responses <- response
				}
			}(popped)
		}

		// Empty the queue.
		queue = []peer.ID{}

		// Wait until all responses from peers come back.
		wait.Wait()

		// Expand nodes in breadth-first search.
		close(responses)
		for response := range responses {
			// Queue up expanded nodes.
			for _, id := range response.Peers {
				peer := peer.ID(*id)

				if _, seen := visited[peer.Hex()]; !seen {
					queue = append(queue, peer)
					visited[peer.Hex()] = struct{}{}

					addresses = append(addresses, peer.Address)

					publicKey := make([]byte, dht.IdSize)
					copy(publicKey, peer.PublicKey[:])

					publicKeys = append(publicKeys, publicKey)
				}
			}
		}
	}

	return
}