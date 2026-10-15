package protocol

import (
	"bytes"
	"errors"
	"io"
)

// StreamHeadClass describes what the head of an upstream SSE stream produced
// before the gateway decided to commit it downstream.
type StreamHeadClass uint8

const (
	// StreamHeadNone means nothing decisive has been seen yet (role chunks,
	// usage frames, comments): a stream that ended here produced no output.
	StreamHeadNone StreamHeadClass = iota
	// StreamHeadContent marks a real content event (text, reasoning, tool
	// call). Once this class is reached the stream is worth committing.
	StreamHeadContent
	// StreamHeadTerminal marks an orderly end of turn without any content:
	// [DONE], a bare finish/stop_reason, or an Anthropic message_stop.
	StreamHeadTerminal
	// StreamHeadError marks an upstream error frame or a frame the parser
	// could not decode.
	StreamHeadError
)

// StreamHeadInspector classifies frames at the head of an upstream stream. It
// wraps the same parser the relay uses, so a stream is judged with exactly the
// rules that will later drive emission.
type StreamHeadInspector struct {
	parser *bridgeStreamParser
}

func NewStreamHeadInspector(from Protocol) *StreamHeadInspector {
	return &StreamHeadInspector{parser: &bridgeStreamParser{
		protocol: from, tools: map[string]bool{}, toolIDs: map[string]string{}, toolNames: map[string]string{},
		responseArgs: map[string]bool{}, responseReasoning: map[string]bool{},
	}}
}

// Feed classifies one frame. The caller stops feeding as soon as the class is
// no longer StreamHeadNone.
func (inspector *StreamHeadInspector) Feed(eventName, data string) StreamHeadClass {
	events, err := inspector.parser.Parse(eventName, data)
	if err != nil {
		return StreamHeadError
	}
	class := StreamHeadNone
	for _, event := range events {
		switch event.Kind {
		case "text", "reasoning", "reasoning_signature", "tool_start", "tool_delta":
			return StreamHeadContent
		case "error":
			return StreamHeadError
		case "finish", "done":
			if class == StreamHeadNone {
				class = StreamHeadTerminal
			}
		}
	}
	return class
}

var errHoldStop = errors.New("stream head satisfied")

// progressReader reports every successful read so callers can drive an idle
// watchdog while the hold is blocked in Read.
type progressReader struct {
	reader io.Reader
	onRead func()
}

func (p progressReader) Read(buffer []byte) (int, error) {
	n, err := p.reader.Read(buffer)
	if n > 0 && p.onRead != nil {
		p.onRead()
	}
	return n, err
}

// HoldFirstContent consumes an upstream SSE stream up to its first content
// event. It returns every byte it read (the prefix) so the caller can either
// replay them into the downstream relay, or discard them and retry the
// request on another node — nothing has been sent downstream while the hold
// runs, which is what makes an invisible retry possible.
//
// Outcomes:
//   - class StreamHeadContent, err nil: commit the attempt; prefix holds the
//     bytes read so far.
//   - class StreamHeadTerminal or StreamHeadError, err nil: the stream ended
//     (or errored) before producing content; the attempt is a zero-output
//     failure and may be retried.
//   - any other error: the stream itself failed (EOF mid-frame, transport
//     error, watchdog abort) before producing content.
//
// onRead, when non-nil, fires after every successful read.
func HoldFirstContent(reader io.Reader, from Protocol, onRead func()) ([]byte, StreamHeadClass, error) {
	source := progressReader{reader: reader, onRead: onRead}
	var captured bytes.Buffer
	inspector := NewStreamHeadInspector(from)
	class := StreamHeadNone
	readErr := readSSE(io.TeeReader(source, &captured), func(eventName, data string) error {
		class = inspector.Feed(eventName, data)
		if class != StreamHeadNone {
			return errHoldStop
		}
		return nil
	})
	prefix := captured.Bytes()
	if errors.Is(readErr, errHoldStop) {
		return prefix, class, nil
	}
	if readErr != nil {
		return prefix, class, readErr
	}
	return prefix, class, nil
}
