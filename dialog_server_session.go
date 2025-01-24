// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/rs/zerolog/log"
)

// DialogServerSession represents inbound channel
type DialogServerSession struct {
	*sipgo.DialogServerSession

	// MediaSession *media.MediaSession
	DialogMedia

	onReferDialog func(referDialog *DialogClientSession)

	mediaConf MediaConfig
	closed    atomic.Uint32
	waitNotify chan error
}

func (d *DialogServerSession) Id() string {
	return d.ID
}

func (d *DialogServerSession) Close() {
	if !d.closed.CompareAndSwap(0, 1) {
		return
	}
	d.DialogMedia.Close()
	d.DialogServerSession.Close()
}

func (d *DialogServerSession) FromUser() string {
	return d.InviteRequest.From().Address.User
}

// User that was dialed
func (d *DialogServerSession) ToUser() string {
	return d.InviteRequest.To().Address.User
}

func (d *DialogServerSession) Transport() string {
	return d.InviteRequest.Transport()
}

func (d *DialogServerSession) Progress() error {
	return d.Respond(sip.StatusTrying, "Trying", nil)
}

func (d *DialogServerSession) Ringing() error {
	return d.Respond(sip.StatusRinging, "Ringing", nil)
}

func (d *DialogServerSession) DialogSIP() *sipgo.Dialog {
	return &d.Dialog
}

func (d *DialogServerSession) RemoteContact() *sip.ContactHeader {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.lastInvite != nil {
		return d.lastInvite.Contact()
	}
	return d.InviteRequest.Contact()
}

func (d *DialogServerSession) Respond(statusCode sip.StatusCode, reason string, body []byte, headers ...sip.Header) error {
	return d.DialogServerSession.Respond(statusCode, reason, body, headers...)
}

func (d *DialogServerSession) RespondSDP(body []byte) error {
	headers := []sip.Header{sip.NewHeader("Content-Type", "application/sdp")}
	return d.DialogServerSession.Respond(200, "OK", body, headers...)
}

// Answer creates media session and answers
// NOTE: Not final API
func (d *DialogServerSession) Answer() error {
	if err := d.initMediaSessionFromConf(d.mediaConf); err != nil {
		return err
	}

	rtpSess := media.NewRTPSession(d.mediaSession)
	return d.answerSession(rtpSess)
}

type AnswerOptions struct {
	// OnMediaUpdate triggers when media update happens. It is blocking func, so make sure you exit
	OnMediaUpdate func(d *DialogMedia)
	OnRefer       func(referDialog *DialogClientSession)
	// Codecs that will be used
	Codecs []media.Codec
}

// AnswerOptions allows to answer dialog with options
// Experimental
//
// NOTE: API may change
func (d *DialogServerSession) AnswerOptions(opt AnswerOptions) error {
	d.mu.Lock()
	d.onReferDialog = opt.OnRefer
	d.onMediaUpdate = opt.OnMediaUpdate
	d.mu.Unlock()

	// Let override of formats
	conf := d.mediaConf
	if opt.Codecs != nil {
		conf.Codecs = opt.Codecs
	}

	if err := d.initMediaSessionFromConf(conf); err != nil {
		return err
	}
	rtpSess := media.NewRTPSession(d.mediaSession)
	return d.answerSession(rtpSess)
}

// answerSession. It allows answering with custom RTP Session.
// NOTE: Not final API
func (d *DialogServerSession) answerSession(rtpSess *media.RTPSession) error {
	sess := rtpSess.Sess
	sdp := d.InviteRequest.Body()
	if sdp == nil {
		return fmt.Errorf("no sdp present in INVITE")
	}

	if err := sess.RemoteSDP(sdp); err != nil {
		return err
	}

	if err := d.RespondSDP(sess.LocalSDP()); err != nil {
		return err
	}

	d.mu.Lock()
	d.initRTPSessionUnsafe(sess, rtpSess)
	// Close RTP session
	d.onCloseUnsafe(func() {
		if err := rtpSess.Close(); err != nil {
			log.Error().Err(err).Msg("Closing session")
		}
	})
	d.mu.Unlock()
	// Must be called after media and reader writer is setup
	rtpSess.MonitorBackground()

	// Wait ACK
	// If we do not wait ACK, hanguping call will fail as ACK can be delayed when we are doing Hangup
	for {
		select {
		case <-time.After(10 * time.Second):
			return fmt.Errorf("no ACK received")
		case state := <-d.StateRead():
			if state == sip.DialogStateConfirmed {
				return nil
			}
			if state == sip.DialogStateEnded {
				return fmt.Errorf("dialog ended on ack")
			}
		}
	}
}

// AnswerLate does answer with Late offer.
func (d *DialogServerSession) AnswerLate() error {
	if err := d.initMediaSessionFromConf(d.mediaConf); err != nil {
		return err
	}
	sess := d.mediaSession
	rtpSess := media.NewRTPSession(sess)
	localSDP := sess.LocalSDP()

	d.mu.Lock()
	d.initRTPSessionUnsafe(sess, rtpSess)
	// Close RTP session
	d.onCloseUnsafe(func() {
		if err := rtpSess.Close(); err != nil {
			log.Error().Err(err).Msg("Closing session")
		}
	})
	d.mu.Unlock()

	if err := d.RespondSDP(localSDP); err != nil {
		return err
	}
	// Wait ACK
	// It is expected that on ReadACK SDP will be updated
	for {
		select {
		case <-time.After(10 * time.Second):
			return fmt.Errorf("no ACK received")
		case state := <-d.StateRead():
			if state == sip.DialogStateConfirmed {
				// Must be called after media and reader writer is setup
				return rtpSess.MonitorBackground()
			}
			if state == sip.DialogStateEnded {
				return fmt.Errorf("dialog ended on ack")
			}
		}
	}
}

func (d *DialogServerSession) ReadAck(req *sip.Request, tx sip.ServerTransaction) error {
	// Check do we have some session
	err := func() error {
		d.mu.Lock()
		defer d.mu.Unlock()
		sess := d.mediaSession
		if sess == nil {
			return nil
		}
		contentType := req.ContentType()
		if contentType == nil {
			return nil
		}
		body := req.Body()
		if body != nil && contentType.Value() == "application/sdp" {
			// This is Late offer response
			if err := sess.RemoteSDP(body); err != nil {
				return err
			}
		}
		return nil
	}()
	if err != nil {
		e := d.Hangup(d.Context())
		return errors.Join(err, e)
	}

	return d.DialogServerSession.ReadAck(req, tx)
}

func (d *DialogServerSession) Hangup(ctx context.Context) error {
	state := d.LoadState()
	if state == sip.DialogStateConfirmed {
		return d.Bye(ctx)
	}
	return d.Respond(sip.StatusTemporarilyUnavailable, "Temporarly unavailable", nil)
}

func (d *DialogServerSession) ReInvite(ctx context.Context) error {
	sdp := d.mediaSession.LocalSDP()
	contact := d.RemoteContact()
	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(sdp)

	res, err := d.Do(ctx, req)
	if err != nil {
		return err
	}

	if !res.IsSuccess() {
		return sipgo.ErrDialogResponse{
			Res: res,
		}
	}
	return nil
}

// Refer tries todo refer (blind transfer) on call
func (d *DialogServerSession) Refer(ctx context.Context, referTo sip.Uri, referredBy sip.Uri) error {
	// TODO check state of call
	if d.LoadState() != sip.DialogStateConfirmed {
		return fmt.Errorf("call not confirmed")
	}

	req := sip.NewRequest(sip.REFER, d.InviteRequest.Contact().Address)
	// UASRequestBuild(req, d.InviteResponse)

	// Invite request tags must be preserved but switched
	req.AppendHeader(sip.NewHeader("Refer-to", fmt.Sprintf("<%s>", referTo.String())))
	req.AppendHeader(sip.NewHeader("Referred-by", fmt.Sprintf("<%s>", referredBy.String())))

	res, err := d.Do(ctx, req)
	if err != nil {
		return err
	}

	if res.StatusCode != sip.StatusAccepted {
		return sipgo.ErrDialogResponse{
			Res: res,
		}
	}

	d.waitNotify = make(chan error)

	select {
	case e := <-d.waitNotify:
		if e != nil {
			return fmt.Errorf("refer failed: %w", e)
		}
		return d.Hangup(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *DialogServerSession) HandleNotify(notifyReq *sip.Request) error {
	body := notifyReq.Body()
	if body == nil {
		return fmt.Errorf("NOTIFY message has no body")
	}

	statusLine := string(body)
    parts := strings.Split(statusLine, " ")

    statusCode, err := strconv.Atoi(parts[1])
    if err != nil {
        return fmt.Errorf("invalid status code in SIP response line: %s", parts[1])
    }

    if d.waitNotify != nil {
        switch statusCode {
        case 100: // 100 Trying
            log.Debug().Msg("Received 100 Trying: request is being processed")
        case 180: // 180 Ringing
            log.Debug().Msg("Received 180 Ringing: target is ringing")
		case 183: // 183 Session Progress
			log.Debug().Msg("Received 183 Session Progress: target is ringing")
        case 200: // 200 OK
            d.waitNotify <- nil // 转接成功
        default:
            d.waitNotify <- fmt.Errorf("refer failed: %s", statusLine)
        }
    }

    return nil
}

// func (d *DialogServerSession) handleReferNotify(req *sip.Request, tx sip.ServerTransaction) {
// 	dialogHandleReferNotify(d, req, tx)

// 	// Invite request tags must be preserved but switched
// 	req.AppendHeader(sip.NewHeader("Refer-to", fmt.Sprintf("<%s>", referTo.String())))
// 	req.AppendHeader(sip.NewHeader("Referred-by", fmt.Sprintf("<%s>", referredBy.String())))

// 	res, err := d.Do(ctx, req)
// 	if err != nil {
// 		return err
// 	}

// 	dialogHandleRefer(d, dg, req, tx, onRefDialog)
// }

// // Refer tries todo refer (blind transfer) on call
// func (d *DialogServerSession) Refer(ctx context.Context, referTo sip.Uri) error {
// 	cont := d.InviteRequest.Contact()
// 	return dialogRefer(ctx, d, cont.Address, referTo)
// }

// func (d *DialogServerSession) handleReferNotify(req *sip.Request, tx sip.ServerTransaction) {
// 	dialogHandleReferNotify(d, req, tx)
// }

func (d *DialogServerSession) handleReInvite(req *sip.Request, tx sip.ServerTransaction) error {
	if err := d.ReadRequest(req, tx); err != nil {
		return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, err.Error(), nil))
	}

	return d.handleMediaUpdate(req, tx)
}

func (d *DialogServerSession) readSIPInfoDTMF(req *sip.Request, tx sip.ServerTransaction) error {
	return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusNotAcceptable, "Not Acceptable", nil))
	// if err := d.ReadRequest(req, tx); err != nil {
	// 	tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
	// 	return
	// }

	// Parse this
	//Signal=1
	// Duration=160
	// reader := bytes.NewReader(req.Body())

	// for {

	// }
}
