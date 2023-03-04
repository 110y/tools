// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bench

import (
	"testing"
)

func BenchmarkHover(b *testing.B) {
	env := repos["tools"].sharedEnv(b)

	env.OpenFile("internal/imports/mod.go")
	loc := env.RegexpSearch("internal/imports/mod.go", "bytes")
	env.Await(env.DoneWithOpen())

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		env.Hover(loc)
	}
}
