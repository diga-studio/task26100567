package forkdb

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"testing"
	"time"
)

const (
	scaleSeedCount = 5000
	scaleRecSize   = 2048
	scaleGens      = 10000
	scaleReads     = 200000
)

// jsonRecord 生成恰好 size 字节的合法 JSON。
func jsonRecord(id string, tag string, size int) []byte {
	head := fmt.Sprintf(`{"id":"%s","tag":"%s","pad":"`, id, tag)
	const tail = `"}`
	padLen := size - len(head) - len(tail)
	if padLen < 0 {
		panic("record size too small")
	}
	b := make([]byte, 0, size)
	b = append(b, head...)
	for i := 0; i < padLen; i++ {
		b = append(b, 'x')
	}
	b = append(b, tail...)
	return b
}

// TestScale 验收规模测试：
//   - 5000 条 × 2048 字节种子；
//   - 连续 10000 代从上一代克隆并定制一条记录，全部历史环境保留可读；
//   - 前 5000 代反复替换 100 个热点标识（各约 50 次）并删除 100 条种子记录，
//     后 5000 代持续新增标识（各 2048 字节）；
//   - 断言总耗时 ≤ 10s，GC 后存活堆 ≤ 192 MiB；
//   - 最深一代 20 万次混合读取 ≤ 5s。
func TestScale(t *testing.T) {
	start := time.Now()

	// 创建种子
	seed := make(map[string][]byte, scaleSeedCount)
	for i := 0; i < scaleSeedCount; i++ {
		id := fmt.Sprintf("seed-%05d", i)
		seed[id] = jsonRecord(id, "seed", scaleRecSize)
	}
	root := New(seed)
	seed = nil // 调用方不再持有输入

	// 连续 10000 代：克隆 + 定制一条
	envs := make([]*Env, 0, scaleGens+1)
	envs = append(envs, root)
	cur := root
	for g := 1; g <= scaleGens; g++ {
		next := cur.Clone()
		switch {
		case g <= 2500: // 反复替换热点标识
			id := fmt.Sprintf("seed-%05d", g%100)
			next.Apply([]Mutation{{ID: id, Data: jsonRecord(id, fmt.Sprintf("gen-%d", g), scaleRecSize)}})
		case g <= 2600: // 删除 100 条种子记录（seed-04900..seed-04999）
			id := fmt.Sprintf("seed-%05d", 4900+(g-2501))
			next.Apply([]Mutation{{ID: id, Delete: true}})
		case g <= 5000: // 继续反复替换热点标识
			id := fmt.Sprintf("seed-%05d", g%100)
			next.Apply([]Mutation{{ID: id, Data: jsonRecord(id, fmt.Sprintf("gen-%d", g), scaleRecSize)}})
		default: // 持续新增标识
			id := fmt.Sprintf("new-%05d", g-5001)
			next.Apply([]Mutation{{ID: id, Data: jsonRecord(id, fmt.Sprintf("gen-%d", g), scaleRecSize)}})
		}
		envs = append(envs, next)
		cur = next
	}
	buildDur := time.Since(start)
	t.Logf("seed + %d gens clone&customize: %v", scaleGens, buildDur)
	if buildDur > 10*time.Second {
		t.Fatalf("build took %v, want <= 10s", buildDur)
	}

	// GC 后测量存活堆（所有历史环境仍持有）
	debug.SetGCPercent(100)
	runtime.GC()
	debug.FreeOSMemory()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	live := ms.HeapAlloc
	t.Logf("live heap after GC: %.1f MiB", float64(live)/(1<<20))
	if live > 192<<20 {
		t.Fatalf("live heap %d bytes, want <= %d", live, uint64(192)<<20)
	}

	// 抽查历史环境仍可读且内容正确
	for _, g := range []int{0, 1, 2500, 2600, 5000, 7500, 9999, 10000} {
		e := envs[g]
		b, ok := e.Get("seed-02000") // 从未修改的种子记录
		if !ok || len(b) != scaleRecSize {
			t.Fatalf("gen %d: untouched seed record broken (ok=%v len=%d)", g, ok, len(b))
		}
	}
	// 删除的种子记录在最深代不存在，在早期代存在
	if _, ok := envs[scaleGens].Get("seed-04950"); ok {
		t.Fatal("deleted seed record should not exist in deepest env")
	}
	if _, ok := envs[2400].Get("seed-04950"); !ok {
		t.Fatal("seed record should exist before its deletion")
	}

	// 最深一代 20 万次混合读取
	deepest := envs[scaleGens]
	readStart := time.Now()
	var sink int
	for i := 0; i < scaleReads; i++ {
		var id string
		switch i % 5 {
		case 0: // 从未修改的种子记录
			id = fmt.Sprintf("seed-%05d", 1000+(i%3000))
		case 1: // 不同代反复替换过的热点记录
			id = fmt.Sprintf("seed-%05d", i%100)
		case 2: // 不同代新增的记录
			id = fmt.Sprintf("new-%05d", i%5000)
		case 3: // 已删除的标识
			id = fmt.Sprintf("seed-%05d", 4900+(i%100))
		case 4: // 从未存在的标识
			id = fmt.Sprintf("ghost-%05d", i%1000)
		}
		b, ok := deepest.Get(id)
		if ok {
			sink += len(b)
		}
	}
	readDur := time.Since(readStart)
	t.Logf("%d reads in deepest env: %v (bytes read: %d)", scaleReads, readDur, sink)
	if readDur > 5*time.Second {
		t.Fatalf("reads took %v, want <= 5s", readDur)
	}

	// 读取结果内容抽查：与预期逐字节一致
	if b, _ := deepest.Get("seed-00007"); string(b[:40]) != string(jsonRecord("seed-00007", "gen-4907", scaleRecSize)[:40]) {
		t.Fatal("hot record content mismatch")
	}
	if b, ok := deepest.Get("new-04999"); !ok || string(b[:30]) != string(jsonRecord("new-04999", "gen-10000", scaleRecSize)[:30]) {
		t.Fatal("last added record content mismatch")
	}
	runtime.KeepAlive(envs)
}
