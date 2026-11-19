package main

import (
	"bufio"
	"bytes"
	"net/textproto"
	"testing"
)

func TestPOPMultiline(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		data string
		size int64
	}{
		{"empty", "", "", 0},
		{"escaped dot", ".foo\r\n.\n", "..foo\r\n..\r\n", 9},
		{"no newline", "abc", "abc\r\n", 5},
		{"ends in carriage return", "abc\r", "abc\r\n", 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			var response bytes.Buffer
			session := popSession{writer: textproto.NewWriter(bufio.NewWriter(&response))}
			if err := session.multiline("Message", []byte(test.raw)); err != nil {
				t.Fatal(err)
			}
			if got, want := response.String(), "+OK Message\r\n"+test.data+".\r\n"; got != want {
				t.Errorf("wire response = %q, want %q", got, want)
			}
			if got := popOctets([]byte(test.raw)); got != test.size {
				t.Errorf("octets = %d, want %d", got, test.size)
			}
		})
	}
}
