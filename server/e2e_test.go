package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/attestantio/go-eth2-client/spec"
	"github.com/flashbots/mev-boost/server/mock"
	"github.com/flashbots/mev-boost/server/params"
	"github.com/holiman/uint256"
	"github.com/prysmaticlabs/go-bitfield"
	"github.com/stretchr/testify/require"

	builderSpec "github.com/attestantio/go-builder-client/spec"
	eth2ApiV1Electra "github.com/attestantio/go-eth2-client/api/v1/electra"

	"github.com/attestantio/go-eth2-client/spec/altair"
	"github.com/attestantio/go-eth2-client/spec/capella"
	"github.com/attestantio/go-eth2-client/spec/deneb"
	"github.com/attestantio/go-eth2-client/spec/electra"
	"github.com/attestantio/go-eth2-client/spec/phase0"
)

func TestMultiRelayPayloadFallback(t *testing.T) {
	const (
		blockHashStr       = "0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7"
		parentHashStr      = blockHashStr
		pubKeyStr          = "0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249"
		simulatedBlockHash = "0x534809bd2b6832edff8d8ce4cb0e50068804fd1ef432c8362ad708a74fdc0e46"
		numRelays          = 2
	)
	relayTimeout := 500 * time.Millisecond
	backend := newTestBackend(t, numRelays, relayTimeout)
	defer closeServers(backend.relays)

	fastRelay := backend.relays[0]
	slowRelay := backend.relays[1]

	fastRelay.ResponseDelay = 0
	slowRelay.ResponseDelay = 300 * time.Millisecond

	fastRelay.GetHeaderResponse = fastRelay.MakeGetHeaderResponse(
		1_000_000_000, blockHashStr, parentHashStr, pubKeyStr, spec.DataVersionElectra,
	)
	slowRelay.GetHeaderResponse = slowRelay.MakeGetHeaderResponse(
		10_000_000_000, blockHashStr, parentHashStr, pubKeyStr, spec.DataVersionElectra,
	)

	slot := uint64(12345)
	parentHash := mock.HexToHash(parentHashStr)
	pubkey := mock.HexToPubkey(pubKeyStr)
	path := getHeaderPath(slot, parentHash, pubkey)

	t.Run("RequestBuilderBids", func(t *testing.T) {
		header := make(http.Header)
		header.Set(HeaderAccept, MediaTypeJSON)

		resp := backend.request(t, http.MethodGet, path, header, nil)
		require.Equal(t, http.StatusOK, resp.Code, "getHeader request failed: %v", resp.Body.String())

		bidResp := new(builderSpec.VersionedSignedBuilderBid)
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &bidResp), "Failed to parse getHeader response")

		expectedHigh := uint256.NewInt(10_000_000_000)
		actualBid := bidResp.Electra.Message.Value
		require.Zero(t, actualBid.Cmp(expectedHigh),
			"expected highest bid %s, got %s", expectedHigh, actualBid)
	})

	t.Run("SimulateBuilderFailure", func(t *testing.T) {
		slot := uint64(12345)
		headerObj := fastRelay.GetHeaderResponse.Electra.Message.Header
		blockHash := mock.HexToHash(simulatedBlockHash)

		signedBlindedBlock := &eth2ApiV1Electra.SignedBlindedBeaconBlock{
			Signature: mock.HexToSignature("0x8c795f751f812eabbabdee85100a06730a9904a4b53eedaa7f546fe0e23cd75125e293c6b0d007aa68a9da4441929d16072668abb4323bb04ac81862907357e09271fe414147b3669509d91d8ffae2ec9c789a5fcd4519629b8f2c7de8d0cce9"),
			Message: &eth2ApiV1Electra.BlindedBeaconBlock{
				Slot:          phase0.Slot(slot),
				ProposerIndex: 1,
				ParentRoot:    phase0.Root(parentHash),
				StateRoot:     phase0.Root{0x02},
				Body: &eth2ApiV1Electra.BlindedBeaconBlockBody{
					RANDAOReveal: phase0.BLSSignature{0xa1},
					ETH1Data: &phase0.ETH1Data{
						BlockHash: blockHash[:],
					},
					Graffiti: phase0.Hash32{0xa2},
					SyncAggregate: &altair.SyncAggregate{
						SyncCommitteeBits: bitfield.NewBitvector512(),
					},
					ProposerSlashings:      []*phase0.ProposerSlashing{},
					Deposits:               []*phase0.Deposit{},
					VoluntaryExits:         []*phase0.SignedVoluntaryExit{},
					ExecutionPayloadHeader: headerObj,
					AttesterSlashings:      []*electra.AttesterSlashing{},
					Attestations:           []*electra.Attestation{},
					BLSToExecutionChanges:  []*capella.SignedBLSToExecutionChange{},
					BlobKZGCommitments:     []deneb.KZGCommitment{},
					ExecutionRequests:      &electra.ExecutionRequests{},
				},
			},
		}
		slowRelay.GetPayloadResponse = blindedBlockToBlockResponse(signedBlindedBlock)

		// Simulate failure of the winning relay before payload request
		slowRelay.Server.Close()

		reqHeader := http.Header{}
		reqHeader.Set("Content-Type", "application/json")
		payloadPath := params.PathGetPayload
		resp2 := backend.request(t, http.MethodPost, payloadPath, reqHeader, signedBlindedBlock)

		// Expect aggregator to return error since builder (relay) is unreachable
		require.Equal(t, http.StatusBadGateway, resp2.Code)
		require.Contains(t, strings.ToLower(resp2.Body.String()), "no successful relay response")
	})
}

func TestSemanticallyInvalidSignedBlindedBlock(t *testing.T) {
	const (
		blockHashStr  = "0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7"
		parentHashStr = blockHashStr
		pubKeyStr     = "0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249"
		numRelays     = 1
	)
	relayTimeout := 500 * time.Millisecond
	backend := newTestBackend(t, numRelays, relayTimeout)
	defer closeServers(backend.relays)

	relay := backend.relays[0]

	relay.GetHeaderResponse = relay.MakeGetHeaderResponse(
		10_000_000_000, blockHashStr, parentHashStr, pubKeyStr, spec.DataVersionElectra,
	)

	slot := uint64(12345)
	parentHash := mock.HexToHash(parentHashStr)
	pubkey := mock.HexToPubkey(pubKeyStr)
	path := getHeaderPath(slot, parentHash, pubkey)

	header := make(http.Header)
	header.Set(HeaderAccept, MediaTypeJSON)

	resp := backend.request(t, http.MethodGet, path, header, nil)
	require.Equal(t, http.StatusOK, resp.Code, "getHeader failed")

	bidResp := new(builderSpec.VersionedSignedBuilderBid)
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &bidResp))

	t.Run("TamperExecutionPayloadHeader", func(t *testing.T) {
		// Tamper the execution header: change the block hash
		tamperedHeader := *(bidResp.Electra.Message.Header)
		tamperedHeader.BlockHash = mock.HexToHash("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

		signed := &eth2ApiV1Electra.SignedBlindedBeaconBlock{
			Signature: mock.HexToSignature("0x8c795f751f812eabbabdee85100a06730a9904a4b53eedaa7f546fe0e23cd75125e293c6b0d007aa68a9da4441929d16072668abb4323bb04ac81862907357e09271fe414147b3669509d91d8ffae2ec9c789a5fcd4519629b8f2c7de8d0cce9"),
			Message: &eth2ApiV1Electra.BlindedBeaconBlock{
				Slot:          phase0.Slot(slot),
				ProposerIndex: 1,
				ParentRoot:    phase0.Root(parentHash),
				StateRoot:     phase0.Root{0x01},
				Body: &eth2ApiV1Electra.BlindedBeaconBlockBody{
					RANDAOReveal: phase0.BLSSignature{0xff},
					ETH1Data: &phase0.ETH1Data{
						BlockHash: tamperedHeader.BlockHash[:],
					},
					Graffiti: phase0.Hash32{0xab},
					SyncAggregate: &altair.SyncAggregate{
						SyncCommitteeBits: bitfield.NewBitvector512(),
					},
					ProposerSlashings:      []*phase0.ProposerSlashing{},
					Deposits:               []*phase0.Deposit{},
					VoluntaryExits:         []*phase0.SignedVoluntaryExit{},
					ExecutionPayloadHeader: &tamperedHeader,
					AttesterSlashings:      []*electra.AttesterSlashing{},
					Attestations:           []*electra.Attestation{},
					BLSToExecutionChanges:  []*capella.SignedBLSToExecutionChange{},
					BlobKZGCommitments:     []deneb.KZGCommitment{},
					ExecutionRequests:      &electra.ExecutionRequests{},
				},
			},
		}
		relay.GetPayloadResponse = blindedBlockToBlockResponse(signed)

		reqHeader := http.Header{}
		reqHeader.Set("Content-Type", "application/json")
		resp2 := backend.request(t, http.MethodPost, params.PathGetPayload, reqHeader, signed)

		require.Equal(t, http.StatusBadGateway, resp2.Code,
			"expected 502 since no relay matched tampered block hash")
		require.Contains(t, resp2.Body.String(), "no successful relay response")
	})

	t.Run("EmptySignature", func(t *testing.T) {
		signed := &eth2ApiV1Electra.SignedBlindedBeaconBlock{
			Signature: phase0.BLSSignature{},
			Message: &eth2ApiV1Electra.BlindedBeaconBlock{
				Slot:          phase0.Slot(slot),
				ProposerIndex: 1,
				ParentRoot:    phase0.Root(parentHash),
				StateRoot:     phase0.Root{0x01},
				Body: &eth2ApiV1Electra.BlindedBeaconBlockBody{
					RANDAOReveal: phase0.BLSSignature{0xff},
					ETH1Data: &phase0.ETH1Data{
						BlockHash: bidResp.Electra.Message.Header.BlockHash[:],
					},
					Graffiti: phase0.Hash32{0xab},
					SyncAggregate: &altair.SyncAggregate{
						SyncCommitteeBits: bitfield.NewBitvector512(),
					},
					ProposerSlashings:      []*phase0.ProposerSlashing{},
					Deposits:               []*phase0.Deposit{},
					VoluntaryExits:         []*phase0.SignedVoluntaryExit{},
					ExecutionPayloadHeader: bidResp.Electra.Message.Header,
					AttesterSlashings:      []*electra.AttesterSlashing{},
					Attestations:           []*electra.Attestation{},
					BLSToExecutionChanges:  []*capella.SignedBLSToExecutionChange{},
					BlobKZGCommitments:     []deneb.KZGCommitment{},
					ExecutionRequests:      &electra.ExecutionRequests{},
				},
			},
		}

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
		resp2 := backend.request(t, http.MethodPost, params.PathGetPayload, reqHeader, signed)

		require.Equal(t, http.StatusBadGateway, resp2.Code,
			"expected 502 since retry limit reached")
		// TODO: Consider improving the error handling in the relay to return a more specific error after retrying
		require.Contains(t, resp2.Body.String(), "could not verify payload signature")
	})
}

func closeServers(relays []*mock.Relay) {
	for _, relay := range relays {
		relay.Server.Close()
	}
}
