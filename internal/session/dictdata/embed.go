// Package dictdata embeds compressed Chinese dictionaries for gse.
// These are gse's zh/s_1.txt and zh/t_1.txt, gzipped to reduce binary size.
// The ne build tag on gse excludes its full embed (which includes an unused
// 23 MB Japanese dictionary); this package re-embeds only the Chinese data.
package dictdata

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"io"
)

//go:embed zh_s.txt.gz
var zhSGz []byte

//go:embed zh_t.txt.gz
var zhTGz []byte

// ZhDict returns the combined simplified + traditional Chinese dictionary
// text, decompressed. Panics on corrupt data (embedded at build time, so
// corruption means a build-system error).
func ZhDict() string {
	return decompress(zhSGz) + decompress(zhTGz)
}

func decompress(gz []byte) string {
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		panic("dictdata: corrupt gzip: " + err.Error())
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		panic("dictdata: gzip read: " + err.Error())
	}
	return string(b)
}
