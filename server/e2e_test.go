package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	builderSpec "github.com/attestantio/go-builder-client/spec"
	eth2ApiV1Electra "github.com/attestantio/go-eth2-client/api/v1/electra"
	"github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/altair"
	"github.com/attestantio/go-eth2-client/spec/capella"
	"github.com/attestantio/go-eth2-client/spec/deneb"
	"github.com/attestantio/go-eth2-client/spec/electra"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/flashbots/mev-boost/server/mock"
	"github.com/flashbots/mev-boost/server/params"
	"github.com/holiman/uint256"
	"github.com/prysmaticlabs/go-bitfield"
	"github.com/stretchr/testify/require"
)

const (
	testBlockHash = "0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7"
	testPubKey    = "0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249"
	testSlot      = uint64(12345)
	testSig       = "0x8c795f751f812eabbabdee85100a06730a9904a4b53eedaa7f546fe0e23cd75125e293c6b0d007aa68a9da4441929d16072668abb4323bb04ac81862907357e09271fe414147b3669509d91d8ffae2ec9c789a5fcd4519629b8f2c7de8d0cce9"
)

func setupTestBackend(t *testing.T, relays int, timeout time.Duration) (*testBackend, func()) {
	t.Helper()
	backend := newTestBackend(t, relays, timeout)
	cleanup := func() {
		closeServers(backend.relays)
	}
	return backend, cleanup
}

func setupBasicRequest(t *testing.T, backend *testBackend) *builderSpec.VersionedSignedBuilderBid {
	t.Helper()
	pubkey := mock.HexToPubkey(testPubKey)
	parentHash := mock.HexToHash(testBlockHash)
	path := getHeaderPath(testSlot, parentHash, pubkey)
	header := http.Header{}
	header.Set(HeaderAccept, MediaTypeJSON)

	resp := backend.request(t, http.MethodGet, path, header, nil)
	require.Equal(t, http.StatusOK, resp.Code)

	bid := new(builderSpec.VersionedSignedBuilderBid)
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), bid))

	return bid
}

func TestMultiRelayPayloadFallback(t *testing.T) {
	backend, cleanup := setupTestBackend(t, 2, 500*time.Millisecond)
	defer cleanup()

	fastRelay := backend.relays[0]
	slowRelay := backend.relays[1]

	fastRelay.ResponseDelay = 0
	slowRelay.ResponseDelay = 300 * time.Millisecond

	fastRelay.GetHeaderResponse = fastRelay.MakeGetHeaderResponse(1_000_000_000, testBlockHash, testBlockHash, testPubKey, spec.DataVersionElectra)
	slowRelay.GetHeaderResponse = slowRelay.MakeGetHeaderResponse(10_000_000_000, testBlockHash, testBlockHash, testPubKey, spec.DataVersionElectra)

	t.Run("RequestBuilderBids", func(t *testing.T) {
		bid := setupBasicRequest(t, backend)
		expected := uint256.NewInt(10_000_000_000)
		actual := bid.Electra.Message.Value
		require.Zero(t, actual.Cmp(expected), "expected highest bid %s, got %s", expected, actual)
	})

	t.Run("SimulateBuilderFailure", func(t *testing.T) {
		header := fastRelay.GetHeaderResponse.Electra.Message.Header
		blockHash := mock.HexToHash("0x534809bd2b6832edff8d8ce4cb0e50068804fd1ef432c8362ad708a74fdc0e46")

		signed := createSignedBlindedBlock(header, blockHash, testSlot, testSig)
		slowRelay.GetPayloadResponse = blindedBlockToBlockResponse(signed)
		slowRelay.Server.Close()

		reqHeader := http.Header{}
		reqHeader.Set("Content-Type", "application/json")
		resp := backend.request(t, http.MethodPost, params.PathGetPayload, reqHeader, signed)

		require.Equal(t, http.StatusBadGateway, resp.Code)
		require.Contains(t, strings.ToLower(resp.Body.String()), "no successful relay response")
	})
}

func TestSemanticallyInvalidSignedBlindedBlock(t *testing.T) {
	backend, cleanup := setupTestBackend(t, 1, 500*time.Millisecond)
	defer cleanup()

	relay := backend.relays[0]
	relay.GetHeaderResponse = relay.MakeGetHeaderResponse(10_000_000_000, testBlockHash, testBlockHash, testPubKey, spec.DataVersionElectra)

	bid := setupBasicRequest(t, backend)

	t.Run("TamperExecutionPayloadHeader", func(t *testing.T) {
		tampered := *bid.Electra.Message.Header
		tampered.BlockHash = mock.HexToHash("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

		signed := createSignedBlindedBlock(&tampered, tampered.BlockHash, testSlot, testSig)
		relay.GetPayloadResponse = blindedBlockToBlockResponse(signed)

		reqHeader := http.Header{}
		reqHeader.Set("Content-Type", "application/json")
		resp := backend.request(t, http.MethodPost, params.PathGetPayload, reqHeader, signed)

		require.Equal(t, http.StatusBadGateway, resp.Code)
		require.Contains(t, resp.Body.String(), "no successful relay response")
	})

	t.Run("EmptySignature", func(t *testing.T) {
		signed := createSignedBlindedBlock(bid.Electra.Message.Header, mock.HexToHash(testBlockHash), testSlot, "")
		relay.OverrideHandleGetPayload(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", MediaTypeJSON)
			w.WriteHeader(http.StatusBadRequest)
			if _, err := w.Write([]byte(`{"code": 400, "message": "could not verify payload signature"}`)); err != nil {
				t.Fatalf("failed to write response: %v", err)
			}
		})
		// Validate the signature
		// https://github.com/flashbots/mev-boost-relay/blob/fdb359fa6b6a7f96d37fb1f8cabb02c3868f965f/services/api/service.go#L654-L660
		relay.OverrideHandleGetPayload(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", MediaTypeJSON)
			w.WriteHeader(http.StatusBadRequest)
			if _, err := w.Write([]byte(`{"code": 400, "message": "could not verify payload signature"}`)); err != nil {
				t.Fatalf("failed to write response: %v", err)
			}
		})

		reqHeader := http.Header{}
		reqHeader.Set("Content-Type", "application/json")
		resp := backend.request(t, http.MethodPost, params.PathGetPayload, reqHeader, signed)

		require.Equal(t, http.StatusBadGateway, resp.Code)
	})
}

func TestSignedBlindedBlockWithSlotMismatch(t *testing.T) {
	backend, cleanup := setupTestBackend(t, 1, 500*time.Millisecond)
	defer cleanup()

	relay := backend.relays[0]
	parentHash := mock.HexToHash(testBlockHash)
	slotMismatch := testSlot + 1

	relay.GetHeaderResponse = relay.MakeGetHeaderResponse(
		10_000_000_000,
		parentHash.String(),
		parentHash.String(),
		testPubKey,
		spec.DataVersionElectra,
	)

	bid := setupBasicRequest(t, backend)
	signed := createSignedBlindedBlock(bid.Electra.Message.Header, parentHash, slotMismatch, testSig)

	reqHeader := http.Header{}
	reqHeader.Set("Content-Type", "application/json")
	resp := backend.request(t, http.MethodPost, params.PathGetPayload, reqHeader, signed)

	require.Equal(t, http.StatusBadGateway, resp.Code)
	require.Contains(t, strings.ToLower(resp.Body.String()), "no successful relay response")
}

func createSignedBlindedBlock(header *deneb.ExecutionPayloadHeader, blockHash phase0.Hash32, slot uint64, hexSig string) *eth2ApiV1Electra.SignedBlindedBeaconBlock {
	sig := phase0.BLSSignature{}
	if hexSig != "" {
		sig = mock.HexToSignature(hexSig)
	}
	return &eth2ApiV1Electra.SignedBlindedBeaconBlock{
		Signature: sig,
		Message: &eth2ApiV1Electra.BlindedBeaconBlock{
			Slot:          phase0.Slot(slot),
			ProposerIndex: 1,
			ParentRoot:    phase0.Root(blockHash),
			StateRoot:     phase0.Root{0x01},
			Body: &eth2ApiV1Electra.BlindedBeaconBlockBody{
				RANDAOReveal: phase0.BLSSignature{0xaa},
				ETH1Data: &phase0.ETH1Data{
					BlockHash: blockHash[:],
				},
				Graffiti: phase0.Hash32{0xbb},
				SyncAggregate: &altair.SyncAggregate{
					SyncCommitteeBits: bitfield.NewBitvector512(),
				},
				ProposerSlashings:      []*phase0.ProposerSlashing{},
				Deposits:               []*phase0.Deposit{},
				VoluntaryExits:         []*phase0.SignedVoluntaryExit{},
				ExecutionPayloadHeader: header,
				AttesterSlashings:      []*electra.AttesterSlashing{},
				Attestations:           []*electra.Attestation{},
				BLSToExecutionChanges:  []*capella.SignedBLSToExecutionChange{},
				BlobKZGCommitments:     []deneb.KZGCommitment{},
				ExecutionRequests:      &electra.ExecutionRequests{},
			},
		},
	}
}

func closeServers(relays []*mock.Relay) {
	for _, relay := range relays {
		relay.Server.Close()
	}
}
