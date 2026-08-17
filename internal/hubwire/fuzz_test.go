package hubwire

import (
	"bytes"
	"testing"
)

func FuzzReader(f *testing.F) {
	f.Add([]byte("{\"t\":\"check\",\"id\":1}\n"))
	f.Add([]byte("{\"t\":\"send\",\"id\":2,\"body_len\":1}\nx"))
	f.Fuzz(func(t *testing.T, data []byte) {
		reader := NewReader(bytes.NewReader(data))
		_, _ = reader.Read()
	})
}
