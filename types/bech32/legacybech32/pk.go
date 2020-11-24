// Deprecated: The module provides legacy bech32 functions which will be removed in a future
// release.
package legacybech32

// nolint

// TODO: remove Bech32 prefix, it's already in package

import (
	"github.com/cosmos/cosmos-sdk/codec/legacy"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/bech32"
)

// Deprecated: Bech32PubKeyType defines a string type alias for a Bech32 public key type.
type Bech32PubKeyType string

// Bech32 conversion constants
// TODO: check where we can remove this
const (
	AccPK   Bech32PubKeyType = "accpub"
	ValPub  Bech32PubKeyType = "valpub"
	ConsPub Bech32PubKeyType = "conspub"
)

// Deprecated: MarshalPubKey returns a Bech32 encoded string containing the appropriate
// prefix based on the key type provided for a given PublicKey.
func MarshalPubKey(pkt Bech32PubKeyType, pubkey cryptotypes.PubKey) (string, error) {
	bech32Prefix := getPrefix(pkt)
	return bech32.ConvertAndEncode(bech32Prefix, legacy.Cdc.MustMarshalBinaryBare(pubkey))
}

// Deprecated: MustMarshalPubKey calls Bech32ifyPubKey and panics on error.
func MustMarshalPubKey(pkt Bech32PubKeyType, pubkey cryptotypes.PubKey) string {
	res, err := MarshalPubKey(pkt, pubkey)
	if err != nil {
		panic(err)
	}

	return res
}

func getPrefix(pkt Bech32PubKeyType) string {
	cfg := sdk.GetConfig()
	switch pkt {
	case AccPK:
		return cfg.GetBech32AccountPubPrefix()

	case ValPub:
		return cfg.GetBech32ValidatorPubPrefix()
	case ConsPub:
		return cfg.GetBech32ConsensusPubPrefix()
	}

	return ""
}

// Deprecated: UnmarshalPubKey returns a PublicKey from a bech32-encoded PublicKey with
// a given key type.
func UnmarshalPubKey(pkt Bech32PubKeyType, pubkeyStr string) (cryptotypes.PubKey, error) {
	bech32Prefix := getPrefix(pkt)

	bz, err := sdk.GetFromBech32(pubkeyStr, bech32Prefix)
	if err != nil {
		return nil, err
	}

	return cryptocodec.PubKeyFromBytes(bz)
}