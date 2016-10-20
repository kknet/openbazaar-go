package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/OpenBazaar/openbazaar-go/ipfs"
	"github.com/OpenBazaar/openbazaar-go/pb"
	"github.com/OpenBazaar/spvwallet"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcutil"
	hd "github.com/btcsuite/btcutil/hdkeychain"
	"github.com/gogo/protobuf/jsonpb"
	"github.com/golang/protobuf/ptypes/timestamp"
	"gx/ipfs/QmT6n4mspWYEya864BhCUJEgyxiRfmiSY9ruQwTUNpRKaM/protobuf/proto"
	crypto "gx/ipfs/QmUWER4r4qMvaCnX5zREcfyiWN7cXN9g3a7fkRqNz8qWPP/go-libp2p-crypto"
	"gx/ipfs/QmYf7ng2hG5XBtJA3tN34DQ2GUN5HNksEw1rLDkmr6vGku/go-multihash"
	"io/ioutil"
	"os"
	"path"
	"time"
)

const (
	RatingMin           = 1
	RatingMax           = 5
	ReviewMaxCharacters = 3000
)

type RatingData struct {
	OrderId         string `json:"orderId"`
	Overall         int    `json:"overall"`
	Quality         int    `json:"quality"`
	Description     int    `json:"description"`
	DeliverySpeed   int    `json:"deliverySpeed"`
	CustomerService int    `json:"customerService"`
	Review          string `json:"rewview"`
	Anonymous       bool   `json:"anonymous"`
}

func (n *OpenBazaarNode) CompleteOrder(ratingData *RatingData, contract *pb.RicardianContract, records []*spvwallet.TransactionRecord) error {

	orderId, err := n.CalcOrderId(contract.BuyerOrder)
	if err != nil {
		return err
	}

	oc := new(pb.OrderCompletion)
	oc.OrderId = orderId

	rating := new(pb.OrderCompletion_Rating)

	rd := new(pb.OrderCompletion_Rating_RatingData)
	rd.RatingKey = contract.BuyerOrder.RatingKey
	if !ratingData.Anonymous {
		rd.BuyerID = contract.BuyerOrder.BuyerID
	}
	rd.VendorID = contract.VendorListings[0].VendorID

	// TODO: when this is a closing of a disputed order we need to use the rating signature
	// from the order confirmation and add the moderators signature
	rd.VendorSig = contract.VendorOrderFulfillment[0].RatingSignature

	rd.Overall = uint32(ratingData.Overall)
	rd.Quality = uint32(ratingData.Quality)
	rd.Description = uint32(ratingData.Description)
	rd.CustomerService = uint32(ratingData.CustomerService)
	rd.DeliverySpeed = uint32(ratingData.DeliverySpeed)
	rd.Review = ratingData.Review

	ts := new(timestamp.Timestamp)
	ts.Seconds = time.Now().Unix()
	ts.Nanos = 0
	rd.Timestamp = ts
	rating.RatingData = rd

	ser, err := proto.Marshal(rating.RatingData)
	if err != nil {
		return err
	}

	ratingKey, err := n.Wallet.MasterPrivateKey().Child(uint32(contract.BuyerOrder.Timestamp.Seconds))
	if err != nil {
		return err
	}
	ecRatingKey, err := ratingKey.ECPrivKey()
	if err != nil {
		return err
	}
	hashed := sha256.Sum256(ser)
	sig, err := ecRatingKey.Sign(hashed[:])
	if err != nil {
		return err
	}
	rating.Signature = sig.Serialize()
	oc.Rating = rating

	// Payout order if moderated
	if contract.BuyerOrder.Payment.Method == pb.Order_Payment_MODERATED {
		var ins []spvwallet.TransactionInput
		var outValue int64
		for _, r := range records {
			if !r.Spent && r.Value > 0 {
				outpointHash, err := hex.DecodeString(r.Txid)
				if err != nil {
					return err
				}
				outValue += r.Value
				in := spvwallet.TransactionInput{OutpointIndex: r.Index, OutpointHash: outpointHash}
				ins = append(ins, in)
			}
		}

		payoutAddress, err := btcutil.DecodeAddress(contract.VendorOrderFulfillment[0].Payout.PayoutAddress, n.Wallet.Params())
		if err != nil {
			return err
		}
		var output spvwallet.TransactionOutput
		outputScript, err := txscript.PayToAddrScript(payoutAddress)
		if err != nil {
			return err
		}
		output.ScriptPubKey = outputScript
		output.Value = outValue

		chaincode, err := hex.DecodeString(contract.BuyerOrder.Payment.Chaincode)
		if err != nil {
			return err
		}
		parentFP := []byte{0x00, 0x00, 0x00, 0x00}
		mPrivKey := n.Wallet.MasterPrivateKey()
		if err != nil {
			return err
		}
		mECKey, err := mPrivKey.ECPrivKey()
		if err != nil {
			return err
		}
		hdKey := hd.NewExtendedKey(
			n.Wallet.Params().HDPrivateKeyID[:],
			mECKey.Serialize(),
			chaincode,
			parentFP,
			0,
			0,
			true)

		buyerKey, err := hdKey.Child(0)
		if err != nil {
			return err
		}
		redeemScript, err := hex.DecodeString(contract.BuyerOrder.Payment.RedeemScript)
		if err != nil {
			return err
		}

		buyerSignatures, err := n.Wallet.CreateMultisigSignature(ins, []spvwallet.TransactionOutput{output}, buyerKey, redeemScript, contract.VendorOrderFulfillment[0].Payout.PayoutFeePerByte)
		if err != nil {
			return err
		}
		var pbSigs []*pb.BitcoinSignature
		for _, s := range buyerSignatures {
			sig := new(pb.BitcoinSignature)
			sig.InputIndex = s.InputIndex
			sig.Signature = s.Signature
			pbSigs = append(pbSigs, sig)
		}
		oc.PayoutSigs = pbSigs
		var vendorSignatures []spvwallet.Signature
		for _, s := range contract.VendorOrderFulfillment[0].Payout.Sigs {
			sig := spvwallet.Signature{InputIndex: s.InputIndex, Signature: s.Signature}
			vendorSignatures = append(vendorSignatures, sig)
		}
		err = n.Wallet.Multisign(ins, []spvwallet.TransactionOutput{output}, buyerSignatures, vendorSignatures, redeemScript, contract.VendorOrderFulfillment[0].Payout.PayoutFeePerByte)
		if err != nil {
			return err
		}
	}

	rc := new(pb.RicardianContract)
	rc.BuyerOrderCompletion = oc

	rc, err = n.SignOrderCompletion(rc)
	if err != nil {
		return err
	}

	err = n.SendOrderCompletion(contract.VendorListings[0].VendorID.Guid, rc)
	if err != nil {
		return err
	}

	contract.BuyerOrderCompletion = oc
	for _, sig := range rc.Signatures {
		if sig.Section == pb.Signatures_ORDER_COMPLETION {
			contract.Signatures = append(contract.Signatures, sig)
		}
	}
	err = n.Datastore.Purchases().Put(orderId, *contract, pb.OrderState_COMPLETE, true)
	if err != nil {
		return err
	}

	return nil
}

func (n *OpenBazaarNode) SignOrderCompletion(contract *pb.RicardianContract) (*pb.RicardianContract, error) {
	serializedOrderFulfil, err := proto.Marshal(contract.BuyerOrderCompletion)
	if err != nil {
		return contract, err
	}
	s := new(pb.Signatures)
	s.Section = pb.Signatures_ORDER_COMPLETION
	if err != nil {
		return contract, err
	}
	guidSig, err := n.IpfsNode.PrivateKey.Sign(serializedOrderFulfil)
	if err != nil {
		return contract, err
	}
	priv, err := n.Wallet.MasterPrivateKey().ECPrivKey()
	if err != nil {
		return contract, err
	}
	hashed := sha256.Sum256(serializedOrderFulfil)
	bitcoinSig, err := priv.Sign(hashed[:])
	if err != nil {
		return contract, err
	}
	s.Guid = guidSig
	s.Bitcoin = bitcoinSig.Serialize()
	contract.Signatures = append(contract.Signatures, s)
	return contract, nil
}

func (n *OpenBazaarNode) ValidateOrderCompletion(contract *pb.RicardianContract) error {
	if err := verifySignaturesOnOrderCompletion(contract); err != nil {
		return err
	}

	// TODO: validate bitcoin signatures of moderated

	return nil
}

func (n *OpenBazaarNode) ValidateAndSaveRating(contract *pb.RicardianContract) error {
	rating := contract.BuyerOrderCompletion.Rating

	pubkey, err := btcec.ParsePubKey(rating.RatingData.RatingKey, btcec.S256())
	if err != nil {
		return err
	}

	signature, err := btcec.ParseSignature(rating.Signature, btcec.S256())
	if err != nil {
		return err
	}

	ser, err := proto.Marshal(rating.RatingData)
	if err != nil {
		return err
	}
	hashed := sha256.Sum256(ser)
	verified := signature.Verify(hashed[:], pubkey)

	if !verified {
		return errors.New("Invalid rating signature on rating")
	}

	ratingSigData, err := proto.Marshal(rating.RatingData.VendorSig.Metadata)
	if err != nil {
		return err
	}
	valid, err := n.IpfsNode.PrivateKey.GetPublic().Verify(ratingSigData, rating.RatingData.VendorSig.Signature)
	if !valid {
		return errors.New("Invalid vendor signature on rating")
	}

	if rating.RatingData.Overall < RatingMin || rating.RatingData.Overall > RatingMax {
		return errors.New("Rating not within valid range")
	}
	if rating.RatingData.Quality < RatingMin || rating.RatingData.Quality > RatingMax {
		return errors.New("Rating not within valid range")
	}
	if rating.RatingData.Description < RatingMin || rating.RatingData.Description > RatingMax {
		return errors.New("Rating not within valid range")
	}
	if rating.RatingData.DeliverySpeed < RatingMin || rating.RatingData.DeliverySpeed > RatingMax {
		return errors.New("Rating not within valid range")
	}
	if rating.RatingData.CustomerService < RatingMin || rating.RatingData.CustomerService > RatingMax {
		return errors.New("Rating not within valid range")
	}

	m := jsonpb.Marshaler{
		EnumsAsInts:  false,
		EmitDefaults: false,
		Indent:       "    ",
		OrigName:     false,
	}
	ratingJson, err := m.MarshalToString(rating)
	if err != nil {
		return err
	}

	sha := sha256.Sum256([]byte(ratingJson))
	h, err := multihash.Encode(sha[:], multihash.SHA2_256)
	if err != nil {
		return err
	}

	mh, err := multihash.Cast(h)
	if err != nil {
		return err
	}

	ratingPath := path.Join(n.RepoPath, "root", "ratings", "rating_"+mh.B58String()[:12])
	f, err := os.Create(ratingPath)
	defer f.Close()
	if err != nil {
		return err
	}

	_, werr := f.Write([]byte(ratingJson))
	if werr != nil {
		return werr
	}

	profile, err := n.GetProfile()
	if err != nil {
		return err
	}
	totalRatingVal := profile.AvgRating * profile.NumRatings
	totalRatingVal += rating.RatingData.Overall
	newAvg := totalRatingVal / (profile.NumRatings + 1)
	profile.AvgRating = newAvg
	profile.NumRatings = profile.NumRatings + 1
	err = n.UpdateProfile(&profile)
	if err != nil {
		return err
	}
	err = n.updateRatingIndex(rating, ratingPath)
	if err != nil {
		return err
	}
	return nil
}

func (n *OpenBazaarNode) updateRatingIndex(rating *pb.OrderCompletion_Rating, ratingPath string) error {
	indexPath := path.Join(n.RepoPath, "root", "ratings", "index.json")

	type ratingShort struct {
		Hash  string   `json:"hash"`
		Slugs []string `json:"slugs"`
	}

	var index []ratingShort

	ratingHash, err := ipfs.AddFile(n.Context, ratingPath)
	if err != nil {
		return err
	}

	rs := ratingShort{
		Hash:  ratingHash,
		Slugs: rating.RatingData.VendorSig.Metadata.ListingSlugs,
	}

	_, ferr := os.Stat(indexPath)
	if !os.IsNotExist(ferr) {
		// Read existing file
		file, err := ioutil.ReadFile(indexPath)
		if err != nil {
			return err
		}
		err = json.Unmarshal(file, &index)
		if err != nil {
			return err
		}
	}

	// Check to see if the rating we are adding already exists in the list. If so delete it.
	for i, d := range index {
		if d.Hash != rs.Hash {
			continue
		}

		if len(index) == 1 {
			index = []ratingShort{}
			break
		}
		index = append(index[:i], index[i+1:]...)
	}

	// Append our rating with the new hash to the list
	index = append(index, rs)

	// Write it back to file
	f, err := os.Create(indexPath)
	defer f.Close()
	if err != nil {
		return err
	}
	j, jerr := json.MarshalIndent(index, "", "    ")
	if jerr != nil {
		return jerr
	}
	_, werr := f.Write(j)
	if werr != nil {
		return werr
	}
	return nil
}

func verifySignaturesOnOrderCompletion(contract *pb.RicardianContract) error {
	guidPubkeyBytes := contract.BuyerOrder.BuyerID.Pubkeys.Guid
	bitcoinPubkeyBytes := contract.BuyerOrder.BuyerID.Pubkeys.Bitcoin
	guid := contract.BuyerOrder.BuyerID.Guid
	ser, err := proto.Marshal(contract.BuyerOrderCompletion)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(ser)
	guidPubkey, err := crypto.UnmarshalPublicKey(guidPubkeyBytes)
	if err != nil {
		return err
	}
	bitcoinPubkey, err := btcec.ParsePubKey(bitcoinPubkeyBytes, btcec.S256())
	if err != nil {
		return err
	}
	var guidSig []byte
	var bitcoinSig *btcec.Signature
	var sig *pb.Signatures
	sigExists := false
	for _, s := range contract.Signatures {
		if s.Section == pb.Signatures_ORDER_COMPLETION {
			sig = s
			sigExists = true
			break
		}
	}
	if !sigExists {
		return errors.New("Contract does not contain a signature for the order completion")
	}
	guidSig = sig.Guid
	bitcoinSig, err = btcec.ParseSignature(sig.Bitcoin, btcec.S256())
	if err != nil {
		return err
	}
	valid, err := guidPubkey.Verify(ser, guidSig)
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("Buyer's guid signature on contact failed to verify")
	}
	checkKeyHash, err := guidPubkey.Hash()
	if err != nil {
		return err
	}
	guidMH, err := multihash.FromB58String(guid)
	if err != nil {
		return err
	}
	if !bytes.Equal(guidMH, checkKeyHash) {
		return errors.New("Public key in order does not match reported buyer ID")
	}
	valid = bitcoinSig.Verify(hash[:], bitcoinPubkey)
	if !valid {
		return errors.New("Buyer's bitcoin signature on contact failed to verify")
	}

	return nil
}