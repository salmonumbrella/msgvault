package testutil

import (
	"bytes"
	_ "embed"
)

//go:embed testdata/pst/synthetic-message.pst
var syntheticPST []byte

//go:embed testdata/pst/synthetic-media.pst
var syntheticPSTMedia []byte

//go:embed testdata/pst/synthetic-broken-media.pst
var syntheticPSTBrokenMedia []byte

//go:embed testdata/pst/synthetic-reply-chain.pst
var syntheticPSTReplyChain []byte

// PSTFixture returns a synthetic email archive for native parser tests.
func PSTFixture() []byte { return bytes.Clone(syntheticPST) }

// PSTMediaFixture returns a synthetic email with one inline archive attachment.
func PSTMediaFixture() []byte { return bytes.Clone(syntheticPSTMedia) }

// PSTBrokenMediaFixture declares an attachment with missing native subnodes.
func PSTBrokenMediaFixture() []byte { return bytes.Clone(syntheticPSTBrokenMedia) }

// PSTReplyChainFixture returns three linked synthetic messages, with a large
// attachment on the intermediate message for retry and threading regressions.
func PSTReplyChainFixture() []byte { return bytes.Clone(syntheticPSTReplyChain) }
