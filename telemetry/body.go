package telemetry

// BodyCapture records the first MaxBodyBytes of a stream and counts the rest.
//
// It is an io.Writer so one implementation covers both halves of a request:
// tee it off the request body on the way in, and off the response on the way
// out. Everything past the cap is counted and thrown away, so a hostile
// multi-megabyte POST costs a fixed 64 KiB of memory and still shows up in the
// data as a large truncated body rather than as nothing.
type BodyCapture struct {
	buf   []byte
	total int64
}

// NewBodyCapture returns an empty capture. The buffer grows to the cap and
// never past it.
func NewBodyCapture() *BodyCapture { return &BodyCapture{} }

// Write never fails and never short-writes: it is a recorder, and a caller
// teeing into it must not start failing because the recorder is full.
func (c *BodyCapture) Write(p []byte) (int, error) {
	c.total += int64(len(p))
	if room := MaxBodyBytes - len(c.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		c.buf = append(c.buf, p[:room]...)
	}
	return len(p), nil
}

// String is the captured prefix.
func (c *BodyCapture) String() string { return string(c.buf) }

// Truncated reports whether anything was thrown away — the flag that keeps a
// capped body honest about being capped.
func (c *BodyCapture) Truncated() bool { return c.total > int64(len(c.buf)) }

// Total is how many bytes went past, captured or not. It is the byte count the
// envelope reports, which stays true even when the body does not.
func (c *BodyCapture) Total() int64 { return c.total }

// CaptureBody is the one-shot form for callers that already hold the bytes.
func CaptureBody(b []byte) (body string, truncated bool) {
	if len(b) > MaxBodyBytes {
		return string(b[:MaxBodyBytes]), true
	}
	return string(b), false
}

// CaptureString is CaptureBody for a value already in a string, avoiding the
// copy when it is already short enough.
func CaptureString(s string) (body string, truncated bool) {
	if len(s) > MaxBodyBytes {
		return s[:MaxBodyBytes], true
	}
	return s, false
}
