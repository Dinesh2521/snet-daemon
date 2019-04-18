//go:generate protoc -I . ./state_service.proto --go_out=plugins=grpc:.

package escrow

import (
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"math/big"
)

// PaymentChannelStateService is an implementation of
// PaymentChannelStateServiceServer gRPC interface
type PaymentChannelStateService struct {
	channelService PaymentChannelService
	paymentStorage   *PaymentStorage
}

// NewPaymentChannelStateService returns new instance of
// PaymentChannelStateService
func NewPaymentChannelStateService(channelService PaymentChannelService, paymentStorage *PaymentStorage,) *PaymentChannelStateService {
	return &PaymentChannelStateService{
		channelService: channelService,
		paymentStorage:paymentStorage,
	}
}

// GetChannelState returns the latest state of the channel which id is passed
// in request. To authenticate sender request should also contain correct
// signature of the channel id.
func (service *PaymentChannelStateService) GetChannelState(context context.Context, request *ChannelStateRequest) (reply *ChannelStateReply, err error) {
	log.WithFields(log.Fields{
		"context": context,
		"request": request,
	}).Debug("GetChannelState called")

	channelID := bytesToBigInt(request.GetChannelId())
	signature := request.GetSignature()
	sender, err := getSignerAddressFromMessage(bigIntToBytes(channelID), signature)
	if err != nil {
		return nil, errors.New("incorrect signature")
	}
	fmt.Printf("\n i am called... ")
	channel, ok, err := service.channelService.PaymentChannel(&PaymentChannelKey{ID: channelID})
	if err != nil {
		return nil, errors.New("channel error:" + err.Error())
	}
	if !ok {
		return nil, fmt.Errorf("channel is not found, channelId: %v", channelID)
	}

	if channel.Signer != *sender {
		return nil, errors.New("only channel signer can get latest channel state")
	}

	fmt.Printf("\n%v", bigIntToBytes(channel.Nonce))
	fmt.Printf("\n%v", bigIntToBytes(channel.AuthorizedAmount))
	fmt.Printf("\n%v", channel.Signature)

	// check if nonce matches with blockchain or not
	nonceEqual, err := service.channelService.StorageNonceMatchesWithBLockchainNonce(&PaymentChannelKey{ID: channelID})
	if err != nil {
		return nil, errors.New("channel nonce comparision error:" + err.Error())
	}
	if !nonceEqual {
		// check for payments in the payment storage with current nonce -1, this will happen if blockchain processed the claim
		// but cli couldn't send the details to daemon update its state
		paymentID := fmt.Sprintf("%v/%v", channel.ChannelID, (&big.Int{}).Sub(channel.Nonce, big.NewInt(1)))
		fmt.Printf("\nchannel storage : noonce %v", (&big.Int{}).Sub(channel.Nonce, big.NewInt(1)))
		payment, ok, err := service.paymentStorage.Get(paymentID)
		if err == nil && ok && payment != nil {
			log.Infof("old payment detected for the channel %v (payments with  nonce = current nonce -1).", channelID)
			fmt.Printf("\n%v", bigIntToBytes(payment.Amount))
			fmt.Printf("\n%v", payment.Signature)

			// return the channel state with old nonce
			return &ChannelStateReply{
				CurrentNonce:         bigIntToBytes(channel.Nonce),
				CurrentSignedAmount:  bigIntToBytes(channel.AuthorizedAmount),
				CurrentSignature:     channel.Signature,
				OldNonceSignedAmount: bigIntToBytes(payment.Amount),
				OldNonceSignature:    payment.Signature,
			}, nil
		}
		return nil, errors.New("unable to extract old payment from storage:" + err.Error())
	}

	if channel.Signature == nil {
		return &ChannelStateReply{
			CurrentNonce: bigIntToBytes(channel.Nonce),
		}, nil
	}

	return &ChannelStateReply{
		CurrentNonce:         bigIntToBytes(channel.Nonce),
		CurrentSignedAmount:  bigIntToBytes(channel.AuthorizedAmount),
		CurrentSignature:     channel.Signature,
	}, nil
}
