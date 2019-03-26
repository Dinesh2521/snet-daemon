//go:generate protoc -I . ./state_service.proto --go_out=plugins=grpc:.

package escrow

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/singnet/snet-daemon/authutils"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

// PaymentChannelStateService is an implementation of
// PaymentChannelStateServiceServer gRPC interface
type PaymentChannelStateService struct {
	channelService PaymentChannelService
}

// NewPaymentChannelStateService returns new instance of
// PaymentChannelStateService
func NewPaymentChannelStateService(channelService PaymentChannelService) *PaymentChannelStateService {
	return &PaymentChannelStateService{
		channelService: channelService,
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
	currentBlock, err := authutils.CurrentBlock()
	if err != nil {
		return nil, errors.New("unable to read current block number")
	}
	message := bytes.Join([][]byte{
		[]byte ("__get_state"),
		channelID.Bytes(),
		abi.U256(currentBlock),
	}, nil)
	signature := request.GetSignature()
	sender, err := authutils.GetSignerAddressFromMessage(message, signature)

	//TODO fall back to older signature versions. this is only to enable backward compatibilty with other components
	if err != nil {
		log.Infof("message does not follow the new signature standard")
	}
	err = nil
	sender, err = authutils.GetSignerAddressFromMessage(bigIntToBytes(channelID), signature)
	if err != nil {
		return nil, errors.New("incorrect signature")
	}

	channel, ok, err := service.channelService.PaymentChannel(&PaymentChannelKey{ID: channelID})
	if err != nil {
		return nil, errors.New("channel error:"+err.Error())
	}
	if !ok {
		return nil, fmt.Errorf("channel is not found, channelId: %v", channelID)
	}

	if channel.Signer != *sender {
		return nil, errors.New("only channel signer can get latest channel state")
	}

	if channel.Signature == nil {
		return &ChannelStateReply{
			CurrentNonce: bigIntToBytes(channel.Nonce),
		}, nil
	}

	return &ChannelStateReply{
		CurrentNonce:        bigIntToBytes(channel.Nonce),
		CurrentSignedAmount: bigIntToBytes(channel.AuthorizedAmount),
		CurrentSignature:    channel.Signature,
		PrevSignedAmount:	 bigIntToBytes(channel.PrevAuthorizedAmount),
		PrevSignature:   	 channel.PrevSignature,
	}, nil
}
