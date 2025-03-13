// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"io"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type RTPDtmfReader struct {
	codec        Codec // Depends on media session. Defaults to 101 per current mapping
	reader       io.Reader
	packetReader *RTPPacketReader

	lastEv  DTMFEvent
	dtmf    rune
	dtmfSet bool
}

// RTP DTMF writer is midleware for reading DTMF events
// It reads from io Reader and checks packet Reader
func NewRTPDTMFReader(codec Codec, packetReader *RTPPacketReader, reader io.Reader) *RTPDtmfReader {
	return &RTPDtmfReader{
		codec:        codec,
		packetReader: packetReader,
		reader:       reader,
		// dmtfs:        make([]rune, 0, 5), // have some
	}
}

// Write is RTP io.Writer which adds more sync mechanism
func (w *RTPDtmfReader) Read(b []byte) (int, error) {
	n, err := w.reader.Read(b)
	if err != nil {
		// Signal our reader that no more dtmfs will be read
		// close(w.dtmfCh)
		return n, err
	}

	// Check is this DTMF
	hdr := w.packetReader.PacketHeader
	if hdr.PayloadType != w.codec.PayloadType {
		return n, nil
	}

	// Now decode DTMF
	ev := DTMFEvent{}
	if err := DTMFDecode(b, &ev); err != nil {
		log.Error().Err(err).Msg("Failed to decode DTMF event")
	}
	w.processDTMFEvent(ev)
	return n, nil
}

func (w *RTPDtmfReader) processDTMFEvent(ev DTMFEvent) {
    if log.Logger.GetLevel() == zerolog.DebugLevel {
        // Expensive call on logger
        log.Debug().Interface("ev", ev).Msg("Processing DTMF event")
    }
    
    // 处理结束事件
    if ev.EndOfEvent {
        // 如果没有前序事件，或者事件类型与上一个不同，则忽略
        if w.lastEv.Duration == 0 || w.lastEv.Event != ev.Event {
            return
        }
        
        // 如果已经设置了dtmf并且是同一个事件，避免重复处理
        if w.dtmfSet && DTMFToRune(ev.Event) == w.dtmf {
            return
        }
        
        dur := ev.Duration - w.lastEv.Duration
        if dur < 160 { // 只要求至少 ~20ms 的持续时间
            log.Debug().Uint16("dur", dur).Msg("Discarded DTMF packet with too short duration")
            return
        }
        
        w.dtmf = DTMFToRune(ev.Event)
        w.dtmfSet = true
        
        // 使用字符形式显示DTMF，确保日志的一致性
        log.Debug().Str("dtmf", string(w.dtmf)).Msg("Received DTMF")
        
        w.lastEv = DTMFEvent{} // 重置上一个事件
        return
    }
    
    // 对于非结束事件的处理逻辑
    // 如果是相同事件的新包，更新持续时间
    if w.lastEv.Event == ev.Event && w.lastEv.Duration > 0 {
        // 只在持续时间增加时更新，避免乱序包问题
        if ev.Duration > w.lastEv.Duration {
            w.lastEv = ev
        }
        return
    }
    
    // 新的 DTMF 事件开始
    // 重置前一个 DTMF 设置状态
    w.dtmfSet = false
    w.lastEv = ev
}

func (w *RTPDtmfReader) ReadDTMF() (rune, bool) {
	defer func() { w.dtmfSet = false }()
	return w.dtmf, w.dtmfSet
	// dtmf, ok := <-w.dtmfCh
	// return DTMFToRune(dtmf), ok
}
