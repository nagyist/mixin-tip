package keeper

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	"github.com/MixinNetwork/tip/crypto"
	"github.com/MixinNetwork/tip/logger"
	"github.com/MixinNetwork/tip/store"
	"github.com/drand/kyber"
)

const (
	EphemeralGracePeriod = time.Hour * 24 * 128
	EphemeralLimitWindow = time.Hour * 24
	EphemeralLimitQuota  = 42
	SecretLimitWindow    = time.Hour * 24 * 7
	SecretLimitQuota     = 10
)

type Response struct {
	Available int
	Nonce     uint64
	Identity  kyber.Point
	Assignor  []byte
	Watcher   []byte
}

func Guard(store store.Storage, priv kyber.Scalar, identity, signature, data string) (*Response, error) {
	b, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil || len(b) == 0 {
		return nil, fmt.Errorf("invalid data %s", data)
	}
	pub, err := crypto.PubKeyFromBase58(identity)
	if err != nil {
		return nil, fmt.Errorf("invalid identity %s", identity)
	}
	b = crypto.DecryptECDH(pub, priv, b)

	var body body
	err = json.Unmarshal(b, &body)
	if err != nil {
		return nil, fmt.Errorf("invalid json %s", string(b))
	}
	if body.Identity != identity {
		return nil, fmt.Errorf("invalid identity %s", identity)
	}
	var ab []byte
	if len(body.Assignee) > 0 {
		ab, err = checkAssignee(body.Assignee)
		if err != nil {
			return nil, err
		}
	}
	eb, valid := new(big.Int).SetString(body.Ephemeral, 16)
	if !valid {
		return nil, fmt.Errorf("invalid ephemeral %s", body.Ephemeral)
	}
	rb, _ := new(big.Int).SetString(body.Rotate, 16)
	sig, err := hex.DecodeString(signature)
	if err != nil {
		return nil, fmt.Errorf("invalid signature %s", signature)
	}

	assignee, err := store.ReadAssignee(crypto.PublicKeyBytes(pub))
	if err != nil {
		return nil, err
	} else if pb := crypto.PublicKeyBytes(pub); assignee != nil && !bytes.Equal(assignee, pb) {
		lkey := append(pb, "SECRET"...)
		available, err := store.CheckLimit(lkey, SecretLimitWindow, SecretLimitQuota, true)
		logger.Debug("keeper.CheckLimit", "ASSIGNEE", true, hex.EncodeToString(assignee), hex.EncodeToString(pb), available, err)
		return &Response{Available: available}, err
	}

	assignor, err := store.ReadAssignor(crypto.PublicKeyBytes(pub))
	if err != nil {
		return nil, err
	} else if assignor == nil {
		assignor = crypto.PublicKeyBytes(pub)
	}

	watcher, _ := hex.DecodeString(body.Watcher)
	if len(watcher) != 32 {
		return nil, fmt.Errorf("invalid watcher %s", body.Watcher)
	}
	oas, og, oc, err := store.Watch(watcher)
	logger.Debug("store.Watch", body.Watcher, hex.EncodeToString(oas), og, oc, err)
	if err != nil {
		return nil, fmt.Errorf("watch %x error %v", watcher, err)
	}
	if oas != nil && !bytes.Equal(oas, assignor) {
		lkey := append(oas, "SECRET"...)
		available, err := store.CheckLimit(lkey, SecretLimitWindow, SecretLimitQuota, true)
		logger.Debug("keeper.CheckLimit", "WATCHER", true, hex.EncodeToString(oas), hex.EncodeToString(assignor), available, err)
		return &Response{Available: available}, err
	}

	lkey := append(assignor, "EPHEMERAL"...)
	available, err := store.CheckLimit(lkey, EphemeralLimitWindow, EphemeralLimitQuota, false)
	if err != nil || available < 1 {
		logger.Debug("keeper.CheckLimit", "EPHEMERAL", false, hex.EncodeToString(assignor), available, err)
		return &Response{Available: available}, err
	}
	nonce, grace := uint64(body.Nonce), time.Duration(body.Grace)
	if grace < EphemeralGracePeriod {
		grace = EphemeralGracePeriod
	}
	valid, err = store.CheckEphemeralNonce(assignor, eb.Bytes(), nonce, grace)
	if err != nil {
		return nil, err
	}
	if !valid {
		available, err = store.CheckLimit(lkey, EphemeralLimitWindow, EphemeralLimitQuota, true)
		logger.Debug("keeper.CheckLimit", "EPHEMERAL", true, hex.EncodeToString(assignor), available, err)
		return &Response{Available: available}, err
	}
	if rb != nil && rb.Sign() > 0 {
		err = store.RotateEphemeralNonce(assignor, rb.Bytes(), nonce)
		if err != nil {
			return nil, err
		}
	}

	lkey = append(assignor, "SECRET"...)
	available, err = store.CheckLimit(lkey, SecretLimitWindow, SecretLimitQuota, false)
	if err != nil || available < 1 {
		logger.Debug("keeper.CheckLimit", "SECRET", false, hex.EncodeToString(assignor), available, err)
		return &Response{Available: available}, err
	}
	err = checkSignature(pub, sig, eb, rb, nonce, uint64(grace), ab)
	if err == nil {
		if len(ab) > 0 {
			err := store.WriteAssignee(assignor, ab[:128])
			logger.Debugf("store.WriteAssignee(%x, %x) => %v", assignor, ab[:128], err)
			if err != nil {
				return nil, err
			}
		}

		return &Response{
			Available: available,
			Nonce:     nonce,
			Identity:  pub,
			Assignor:  assignor,
			Watcher:   watcher,
		}, nil
	}
	available, err = store.CheckLimit(lkey, SecretLimitWindow, SecretLimitQuota, true)
	logger.Debug("keeper.CheckLimit", "SECRET", true, hex.EncodeToString(assignor), available, err)
	return &Response{Available: available}, err
}

func checkSignature(pub kyber.Point, sig []byte, eb, rb *big.Int, nonce, grace uint64, ab []byte) error {
	if len(eb.Bytes()) > 32 || eb.Sign() <= 0 {
		return fmt.Errorf("invalid ephemeral %x", eb.Bytes())
	}
	msg := crypto.PublicKeyBytes(pub)
	ebuf := make([]byte, 32)
	eb.FillBytes(ebuf)
	msg = append(msg, ebuf...)
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, nonce)
	msg = append(msg, buf...)
	binary.BigEndian.PutUint64(buf, grace)
	msg = append(msg, buf...)
	if rb != nil && rb.Sign() > 0 && len(rb.Bytes()) <= 32 {
		rbuf := make([]byte, 32)
		rb.FillBytes(rbuf)
		msg = append(msg, rbuf...)
	}
	msg = append(msg, ab...)
	return crypto.Verify(pub, msg, sig)
}

func checkAssignee(as string) ([]byte, error) {
	ab, err := hex.DecodeString(as)
	if err != nil {
		return nil, fmt.Errorf("invalid assignee format %s", err)
	}
	if len(ab) != 192 {
		return nil, fmt.Errorf("invalid assignee format %d", len(as))
	}
	ap, err := crypto.PubKeyFromBytes(ab[:128])
	if err != nil {
		return nil, fmt.Errorf("invalid assignee public key %s", err)
	}
	return ab, crypto.Verify(ap, ab[:128], ab[128:])
}

type body struct {
	// main identity public key to check signature
	Identity string `json:"identity"`

	// a new identity to represent the main identity
	Assignee string `json:"assignee"`

	// the ephemeral secret to authenticate
	Ephemeral string `json:"ephemeral"`

	// ephemeral grace period to maintain the secret valid, the grace
	// will be extended for each valid request, and if the grace expired
	// the ephemeral can be reset
	Grace int64 `json:"grace"`

	// ensure each request can only be used once
	Nonce int64 `json:"nonce"`

	// the ephemeral rotation allows user to use a new secret
	// e.g. when they cooperate with others to generate a non-random
	// ephemeral to replace their on-device random one
	Rotate string `json:"rotate"`

	// the key to watch the identity state
	Watcher string `json:"watcher"`
}
