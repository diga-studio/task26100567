package forkdb

import (
	"bytes"
	"fmt"
	"runtime"
	"testing"
	"time"
)

const (
	scaleSeedCount   = 5000
	scaleRecordSize  = 2048
	scaleGenerations = 10000
	scaleReads       = 200000
)

// makeRecord 生成恰好 2048 字节的有效 JSON 记录。
func makeRecord(id string, seq int) []byte {
	head := fmt.Sprintf(`{"id":%q,"seq":%d,"payload":"`, id, seq)
	const tail = `"}`
	pad := scaleRecordSize - len(head) - len(tail)
	if pad < 0 {
		panic("record header too long")
	}
	b := make([]byte, 0, scaleRecordSize)
	b = append(b, head...)
	for i := 0; i < pad; i++ {
		b = append(b, byte('a'+i%26))
	}
	return append(b, tail...)
}

// 规模测试：5000 条 × 2048 字节种子，连续 10000 代克隆并定制一条记录，
// 覆盖“反复替换已有标识”和“持续新增标识”两类负载（另穿插删除），
// 全部历史环境保留；校验总耗时、GC 后存活堆内存与最深代 20 万次读取耗时。
func TestScale(t *testing.T) {
	seed := make(map[string][]byte, scaleSeedCount)
	for i := 0; i < scaleSeedCount; i++ {
		seed[fmt.Sprintf("seed-%05d", i)] = makeRecord(fmt.Sprintf("seed-%05d", i), 0)
	}

	start := time.Now()
	envs := make([]*Environment, 0, scaleGenerations+1)
	envs = append(envs, NewEnvironment(seed))
	seed = nil // 调用方不再持有种子输入

	// 跟踪最深代的期望状态：替换/新增的最新 seq，以及被删除的标识。
	latestSeq := map[string]int{} // 被替换的种子记录与新记录 → 最新 seq
	deleted := map[string]bool{}  // 被删除的种子记录
	for g := 1; g <= scaleGenerations; g++ {
		child := envs[g-1].Clone()
		var ch Change
		switch g % 3 {
		case 0: // 反复替换已有标识（200 个标识循环，每个约被替换 16 次）
			id := fmt.Sprintf("seed-%05d", (g/3)%200)
			ch = Change{ID: id, Data: makeRecord(id, g)}
			latestSeq[id] = g
		case 1: // 持续新增标识
			id := fmt.Sprintf("new-%05d", g)
			ch = Change{ID: id, Data: makeRecord(id, g)}
			latestSeq[id] = g
		case 2: // 删除种子记录（另一段 200 个标识循环）
			id := fmt.Sprintf("seed-%05d", 200+(g/3)%200)
			ch = Change{ID: id, Delete: true}
			deleted[id] = true
		}
		child.Apply([]Change{ch})
		envs = append(envs, child)
	}
	buildElapsed := time.Since(start)

	// 历史环境始终保留且可读取：抽查首代、中间代。
	if v, ok := envs[0].Read("seed-00000"); !ok || !bytes.Equal(v, makeRecord("seed-00000", 0)) {
		t.Fatalf("generation 0 seed record unreadable")
	}
	if _, ok := envs[0].Read("new-00001"); ok {
		t.Fatalf("generation 0 must not see later additions")
	}
	mid := envs[scaleGenerations/2]
	if _, ok := mid.Read(fmt.Sprintf("new-%05d", scaleGenerations)); ok {
		t.Fatalf("middle generation must not see later additions")
	}
	if v, ok := mid.Read(fmt.Sprintf("new-%05d", 3*(scaleGenerations/6)+1)); !ok || v == nil {
		t.Fatalf("middle generation lost its own addition")
	}

	// GC 后测量存活堆内存（所有历史环境仍被持有）。
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	liveHeap := ms.HeapAlloc

	// 最深代 20 万次单条读取，覆盖：从未修改的种子记录、不同代替换的记录、
	// 不同代新增的记录、已删除标识、从未存在的标识。
	deepest := envs[scaleGenerations]
	replacedIDs := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		replacedIDs = append(replacedIDs, fmt.Sprintf("seed-%05d", i))
	}
	deletedIDs := make([]string, 0, 200)
	for i := 200; i < 400; i++ {
		deletedIDs = append(deletedIDs, fmt.Sprintf("seed-%05d", i))
	}

	readStart := time.Now()
	for i := 0; i < scaleReads; i++ {
		switch i % 5 {
		case 0: // 从未修改的种子记录（400 号段之后未被替换/删除触碰）
			id := fmt.Sprintf("seed-%05d", 400+i%(scaleSeedCount-400))
			if v, ok := deepest.Read(id); !ok || !bytes.Equal(v, makeRecord(id, 0)) {
				t.Fatalf("untouched seed record %q mismatch", id)
			}
		case 1: // 不同代反复替换的记录，应读到最新内容
			id := replacedIDs[i%len(replacedIDs)]
			if v, ok := deepest.Read(id); !ok || !bytes.Equal(v, makeRecord(id, latestSeq[id])) {
				t.Fatalf("replaced record %q mismatch", id)
			}
		case 2: // 不同代新增的记录
			g := 1 + 3*(i%(scaleGenerations/3))
			id := fmt.Sprintf("new-%05d", g)
			if v, ok := deepest.Read(id); !ok || !bytes.Equal(v, makeRecord(id, g)) {
				t.Fatalf("added record %q mismatch", id)
			}
		case 3: // 已删除的标识
			if _, ok := deepest.Read(deletedIDs[i%len(deletedIDs)]); ok {
				t.Fatalf("deleted record readable")
			}
		case 4: // 从未存在的标识
			if _, ok := deepest.Read(fmt.Sprintf("ghost-%07d", i)); ok {
				t.Fatalf("ghost record readable")
			}
		}
	}
	readElapsed := time.Since(readStart)
	runtime.KeepAlive(envs)

	t.Logf("build: %d generations in %v (limit 10s)", scaleGenerations, buildElapsed)
	t.Logf("live heap after GC: %.1f MiB (limit 192 MiB)", float64(liveHeap)/1024/1024)
	t.Logf("reads: %d lookups in %v (limit 5s)", scaleReads, readElapsed)

	if buildElapsed > 10*time.Second {
		t.Fatalf("build too slow: %v", buildElapsed)
	}
	if liveHeap > 192<<20 {
		t.Fatalf("live heap too large: %.1f MiB", float64(liveHeap)/1024/1024)
	}
	if readElapsed > 5*time.Second {
		t.Fatalf("reads too slow: %v", readElapsed)
	}
	if !deleted[deletedIDs[0]] || latestSeq[replacedIDs[0]] == 0 {
		t.Fatalf("workload bookkeeping broken")
	}
}
