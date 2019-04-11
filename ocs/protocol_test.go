package ocs

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.dedis.ch/cothority/v3"
	dkgprotocol "go.dedis.ch/cothority/v3/dkg/pedersen"
	"go.dedis.ch/kyber/v3"
	"go.dedis.ch/kyber/v3/share"
	dkg "go.dedis.ch/kyber/v3/share/dkg/pedersen"
	"go.dedis.ch/kyber/v3/suites"
	"go.dedis.ch/kyber/v3/util/key"
	"go.dedis.ch/kyber/v3/util/random"
	"go.dedis.ch/onet/v3"
	"go.dedis.ch/onet/v3/log"
)

var tSuite = cothority.Suite

// Used for tests
var testServiceID onet.ServiceID

const testServiceName = "ServiceOCS"

func init() {
	var err error
	testServiceID, err = onet.RegisterNewService(testServiceName, newTestService)
	log.ErrFatal(err)
}

// Tests a 3, 5 and 13-node system.
func TestOCS(t *testing.T) {
	nodes := []int{3}
	// nodes := []int{3, 5, 10}
	for _, nbrNodes := range nodes {
		log.Lvlf1("Starting setupDKG with %d nodes", nbrNodes)
		ocs(t, nbrNodes, nbrNodes-1, 29, 0, false)
	}
}

// Tests a system with failing nodes
func TestFail(t *testing.T) {
	ocs(t, 4, 2, 29, 2, false)
}

// Tests what happens if the nodes refuse to send their share
func TestRefuse(t *testing.T) {
	log.Lvl1("Starting setupDKG with 3 nodes and refusing to sign")
	ocs(t, 3, 2, 29, 0, true)
}

func TestOCSKeyLengths(t *testing.T) {
	if testing.Short() {
		t.Skip("Testing all keylengths takes some time...")
	}
	for keylen := 1; keylen <= 29; keylen += 2 {
		log.Lvl1("Testing keylen of", keylen)
		ocs(t, 3, 2, keylen, 0, false)
	}
}

var suite = suites.MustFind("Ed25519")

func TestOnchain(t *testing.T) {
	// 1 - share generation
	nbrPeers := 5
	threshold := 3
	dkgs, err := CreateDKGs(suite.(dkg.Suite), nbrPeers, threshold)
	log.ErrFatal(err)

	// Get aggregate public share
	dks, err := dkgs[0].DistKeyShare()
	log.ErrFatal(err)
	X := dks.Public()

	// 5.1.2 - Encryption
	data := []byte("Very secret Message to be encrypted")
	var k [16]byte
	random.Bytes(k[:], random.New())

	encData, err := aeadSeal(k[:], data)
	if err != nil {
		t.Fatal(err)
	}
	U, C, err := EncodeKey(suite, X, k[:])
	require.NoError(t, err)
	// U and C is shared with everybody

	// Reader's keypair
	xc := key.NewKeyPair(cothority.Suite)

	// Decryption
	Ui := make([]*share.PubShare, nbrPeers)
	for i := range Ui {
		dks, err := dkgs[i].DistKeyShare()
		log.ErrFatal(err)
		v := suite.Point().Mul(dks.Share.V, U)
		v.Add(v, suite.Point().Mul(dks.Share.V, xc.Public))
		Ui[i] = &share.PubShare{
			I: i,
			V: v,
		}
	}

	// XhatEnc is the re-encrypted share under the reader's public key
	XhatEnc, err := share.RecoverCommit(suite, Ui, threshold, nbrPeers)
	log.ErrFatal(err)

	// Decrypt XhatEnc
	keyHat, err := DecodeKey(suite, X, C, XhatEnc, xc.Private)
	log.ErrFatal(err)

	// Extract the message - keyHat is the recovered key
	log.Lvl2(encData)
	dataHat, err := aeadOpen(keyHat, encData)
	if err != nil {
		t.Fatal(err)
	}
	require.Equal(t, data, dataHat)
	log.Lvl1("Original data", string(data))
	log.Lvl1("Recovered data", string(dataHat))
}

// CreateDKGs is used for testing to set up a set of DKGs.
//
// Input:
//   - suite - the suite to use
//   - nbrNodes - how many nodes to set up
//   - threshold - how many nodes can recover the secret
//
// Output:
//   - dkgs - a slice of dkg-structures
//   - err - an eventual error
func CreateDKGs(suite dkg.Suite, nbrNodes, threshold int) (dkgs []*dkg.DistKeyGenerator, err error) {
	// 1 - share generation
	dkgs = make([]*dkg.DistKeyGenerator, nbrNodes)
	scalars := make([]kyber.Scalar, nbrNodes)
	points := make([]kyber.Point, nbrNodes)
	// 1a - initialisation
	for i := range scalars {
		scalars[i] = suite.Scalar().Pick(suite.RandomStream())
		points[i] = suite.Point().Mul(scalars[i], nil)
	}

	// 1b - key-sharing
	for i := range dkgs {
		dkgs[i], err = dkg.NewDistKeyGenerator(suite,
			scalars[i], points, threshold)
		if err != nil {
			return
		}
	}
	// Exchange of Deals
	responses := make([][]*dkg.Response, nbrNodes)
	for i, p := range dkgs {
		responses[i] = make([]*dkg.Response, nbrNodes)
		deals, err := p.Deals()
		if err != nil {
			return nil, err
		}
		for j, d := range deals {
			responses[i][j], err = dkgs[j].ProcessDeal(d)
			if err != nil {
				return nil, err
			}
		}
	}
	// ProcessResponses
	for i, resp := range responses {
		for j, r := range resp {
			for k, p := range dkgs {
				if r != nil && j != k {
					log.Lvl3("Response from-to-peer:", i, j, k)
					justification, err := p.ProcessResponse(r)
					if err != nil {
						return nil, err
					}
					if justification != nil {
						return nil, errors.New("there should be no justification")
					}
				}
			}
		}
	}

	// Verify if all is OK
	for _, p := range dkgs {
		if !p.Certified() {
			return nil, errors.New("one of the dkgs is not finished yet")
		}
	}
	return
}

// These functions encapsulate the kind-of messy-to-use
// Go stdlib AEAD functions. We used to use the AEAD from crypto.v0,
// but it has been removed in preference to the standard one for now.
//
// If we want to use it in more places, it should be cleaned up,
// and moved to a permanent home.

// This suggested length is from https://godoc.org/crypto/cipher#NewGCM example
const nonceLen = 12

func aeadSeal(symKey, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(symKey)
	if err != nil {
		return nil, err
	}

	// Never use more than 2^32 random nonces with a given key because of the risk of a repeat.
	nonce := make([]byte, nonceLen)
	_, err = io.ReadFull(rand.Reader, nonce)
	if err != nil {
		return nil, err
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	encData := aesgcm.Seal(nil, nonce, data, nil)
	encData = append(encData, nonce...)
	return encData, nil
}

func aeadOpen(key, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	log.ErrFatal(err)

	if len(ciphertext) < 12 {
		return nil, errors.New("ciphertext too short")
	}
	nonce := ciphertext[len(ciphertext)-nonceLen:]
	out, err := aesgcm.Open(nil, nonce, ciphertext[0:len(ciphertext)-nonceLen], nil)
	return out, err
}

func ocs(t *testing.T, nbrNodes, threshold, keylen, fail int, refuse bool) {
	local := onet.NewLocalTest(tSuite)
	defer local.CloseAll()
	servers, _, tree := local.GenBigTree(nbrNodes, nbrNodes, nbrNodes, true)
	log.Lvl3(tree.Dump())

	// 1 - setting up - in real life uses Setup-protocol
	// Store the dkgs in the services
	dkgs, err := CreateDKGs(tSuite.(dkg.Suite), nbrNodes, threshold)
	require.Nil(t, err)
	services := local.GetServices(servers, testServiceID)
	for i := range services {
		services[i].(*testService).Shared, _, err = dkgprotocol.NewSharedSecret(dkgs[i])
		require.Nil(t, err)
	}

	// Get the collective public key
	dks, err := dkgs[0].DistKeyShare()
	require.Nil(t, err)
	X := dks.Public()

	// 2 - writer - Encrypt a symmetric key and publish U, C
	k := make([]byte, keylen)
	random.Bytes(k, random.New())
	U, C, err := EncodeKey(tSuite, X, k)
	require.NoError(t, err)

	// 3 - reader - Makes a request to U by giving his public key Xc
	// xc is the client's private/publick key pair
	xc := key.NewKeyPair(cothority.Suite)

	// 4 - service - starts the protocol -
	// as every node needs to have its own DKG, we
	// use a service to give the corresponding DKGs to the nodes.

	// First stop the nodes that should fail
	for _, s := range servers[1 : 1+fail] {
		log.Lvl1("Pausing", s.ServerIdentity)
		s.Pause()
	}
	pi, err := services[0].(*testService).createOCS(tree, threshold)
	require.Nil(t, err)
	protocol := pi.(*OCS)
	protocol.U = U
	protocol.Xc = xc.Public
	protocol.Poly = share.NewPubPoly(suite, suite.Point().Base(), dks.Commits)
	if !refuse {
		protocol.VerificationData = []byte("correct block")
	}
	// timeout := network.WaitRetry * time.Duration(network.MaxRetryConnect*nbrNodes*2) * time.Millisecond
	require.Nil(t, protocol.Start())
	select {
	case <-protocol.Reencrypted:
		log.Lvl2("root-node is done")
		// Wait for other nodes
	case <-time.After(time.Second):
		t.Fatal("Didn't finish in time")
	}

	// 5 - service - Lagrange interpolate the Uis - the reader will only
	// get XhatEnc
	var XhatEnc kyber.Point
	if refuse {
		require.Nil(t, protocol.Uis, "Reencrypted request that should've been refused")
		return
	}

	require.NotNil(t, protocol.Uis)
	XhatEnc, err = share.RecoverCommit(suite, protocol.Uis, threshold, nbrNodes)
	require.Nil(t, err, "Reencryption failed")

	// 6 - reader - gets the resulting symmetric key, encrypted under Xc
	keyHat, err := DecodeKey(suite, X, C, XhatEnc, xc.Private)
	require.Nil(t, err)

	require.Equal(t, k, keyHat)
}

// testService allows setting the dkg-field of the protocol.
type testService struct {
	// We need to embed the ServiceProcessor, so that incoming messages
	// are correctly handled.
	*onet.ServiceProcessor

	// Has to be initialised by the test
	Shared *dkgprotocol.SharedSecret
	Poly   *share.PubPoly
}

// Creates a service-protocol and returns the ProtocolInstance.
func (s *testService) createOCS(t *onet.Tree, threshold int) (onet.ProtocolInstance, error) {
	pi, err := s.CreateProtocol(NameOCS, t)
	pi.(*OCS).Shared = s.Shared
	pi.(*OCS).Poly = s.Poly
	pi.(*OCS).Threshold = threshold
	return pi, err
}

// Store the dkg in the protocol
func (s *testService) NewProtocol(tn *onet.TreeNodeInstance, conf *onet.GenericConfig) (onet.ProtocolInstance, error) {
	switch tn.ProtocolName() {
	case NameOCS:
		pi, err := NewOCS(tn)
		if err != nil {
			return nil, err
		}
		ocs := pi.(*OCS)
		ocs.Shared = s.Shared
		ocs.Verify = func(rc *MessageReencrypt) bool {
			return rc.VerificationData != nil
		}
		return ocs, nil
	default:
		return nil, errors.New("unknown protocol for this service")
	}
}

// EncodeKey can be used by the writer to an onchain-secret skipchain
// to encode his symmetric key under the collective public key created
// by the DKG.
// As this method uses `Pick` to encode the key, depending on the key-length
// more than one point is needed to encode the data.
//
// Input:
//   - suite - the cryptographic suite to use
//   - X - the aggregate public key of the DKG
//   - key - the symmetric key for the document
//
// Output:
//   - U - the schnorr commit
//   - C - encrypted key
func EncodeKey(suite suites.Suite, X kyber.Point, key []byte) (U kyber.Point, C kyber.Point, err error) {
	if len(key) > suite.Point().EmbedLen() {
		return nil, nil, errors.New("got more data than can fit into one point")
	}
	r := suite.Scalar().Pick(suite.RandomStream())
	C = suite.Point().Mul(r, X)
	log.Lvl3("C:", C.String())
	U = suite.Point().Mul(r, nil)
	log.Lvl3("U is:", U.String())

	kp := suite.Point().Embed(key, suite.RandomStream())
	log.Lvl3("Keypoint:", kp.String())
	log.Lvl3("X:", X.String())
	C.Add(C, kp)
	return
}

// DecodeKey can be used by the reader of an onchain-secret to convert the
// re-encrypted secret back to a symmetric key that can be used later to
// decode the document.
//
// Input:
//   - suite - the cryptographic suite to use
//   - X - the aggregate public key of the DKG
//   - C - the encrypted key
//   - XhatEnc - the re-encrypted schnorr-commit
//   - xc - the private key of the reader
//
// Output:
//   - key - the re-assembled key
//   - err - an eventual error when trying to recover the data from the points
func DecodeKey(suite kyber.Group, X kyber.Point, C kyber.Point, XhatEnc kyber.Point,
	xc kyber.Scalar) (key []byte, err error) {
	log.Lvl3("xc:", xc)
	xcInv := suite.Scalar().Neg(xc)
	log.Lvl3("xcInv:", xcInv)
	sum := suite.Scalar().Add(xc, xcInv)
	log.Lvl3("xc + xcInv:", sum, "::", xc)
	log.Lvl3("X:", X)
	XhatDec := suite.Point().Mul(xcInv, X)
	log.Lvl3("XhatDec:", XhatDec)
	log.Lvl3("XhatEnc:", XhatEnc)
	Xhat := suite.Point().Add(XhatEnc, XhatDec)
	log.Lvl3("Xhat:", Xhat)
	XhatInv := suite.Point().Neg(Xhat)
	log.Lvl3("XhatInv:", XhatInv)

	// Decrypt C to keyPointHat
	log.Lvl3("C:", C)
	keyPointHat := suite.Point().Add(C, XhatInv)
	log.Lvl3("keyPointHat:", keyPointHat)
	key, err = keyPointHat.Data()
	if err != nil {
		return nil, erret(err)
	}
	log.Lvl3("key:", key)
	return
}

// starts a new service. No function needed.
func newTestService(c *onet.Context) (onet.Service, error) {
	s := &testService{
		ServiceProcessor: onet.NewServiceProcessor(c),
	}
	return s, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
