package forkdb

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

func makeSeed(ids ...string) map[string][]byte {
	seed := make(map[string][]byte, len(ids))
	for _, id := range ids {
		seed[id] = []byte(fmt.Sprintf(`{"id":%q,"origin":"seed"}`, id))
	}
	return seed
}

func mustRead(t *testing.T, e *Environment, id string) ([]byte, bool) {
	t.Helper()
	return e.Read(id)
}

func assertAbsent(t *testing.T, e *Environment, id string) {
	t.Helper()
	if v, ok := e.Read(id); ok {
		t.Fatalf("expected %q absent, got %q", id, v)
	}
}

func assertContent(t *testing.T, e *Environment, id string, want []byte) {
	t.Helper()
	v, ok := e.Read(id)
	if !ok {
		t.Fatalf("expected %q present", id)
	}
	if !bytes.Equal(v, want) {
		t.Fatalf("%q: got %q want %q", id, v, want)
	}
}

// 多代分叉中，被删除的记录不得在后代中复活。
func TestDeleteDoesNotResurrect(t *testing.T) {
	seed := makeSeed("a", "b", "c")
	root := NewEnvironment(seed)

	g1 := root.Clone()
	g1.Apply([]Change{{ID: "b", Delete: true}})

	g2 := g1.Clone()
	g2.Apply([]Change{{ID: "x", Data: []byte(`{"n":1}`)}})

	g3 := g2.Clone()
	g3.Apply([]Change{{ID: "c", Data: []byte(`{"n":2}`)}})

	g4 := g3.Clone() // 无修改直克隆

	// 源环境仍有 b；所有后代均不得读到 b。
	assertAbsent(t, g4, "b")
	assertAbsent(t, g3, "b")
	assertAbsent(t, g2, "b")
	assertAbsent(t, g1, "b")
	if v, ok := mustRead(t, root, "b"); !ok || string(v) != `{"id":"b","origin":"seed"}` {
		t.Fatalf("root lost seed record b: %q ok=%v", v, ok)
	}
	// 后代各自的定制都在。
	assertContent(t, g4, "x", []byte(`{"n":1}`))
	assertContent(t, g4, "c", []byte(`{"n":2}`))
	assertContent(t, g2, "c", []byte(`{"id":"c","origin":"seed"}`))

	// 在删除后继续沿链克隆并做其它修改，b 仍不得复活。
	g5 := g4.Clone()
	g5.Apply([]Change{{ID: "a", Data: []byte(`{"n":3}`)}})
	assertAbsent(t, g5, "b")
	assertContent(t, g5, "a", []byte(`{"n":3}`))
	assertContent(t, g4, "a", []byte(`{"id":"a","origin":"seed"}`))

	// 同一批次内先替换后删除另一标识，删除仍然生效。
	g6 := g5.Clone()
	g6.Apply([]Change{
		{ID: "x", Data: []byte(`{"n":9}`)},
		{ID: "a", Delete: true},
	})
	assertAbsent(t, g6, "a")
	assertContent(t, g6, "x", []byte(`{"n":9}`))
	assertContent(t, g5, "a", []byte(`{"n":3}`))
	assertContent(t, g5, "x", []byte(`{"n":1}`))
}

// 重置恢复整条链最初创建时的种子集，而不是克隆时的定制状态，且不影响旁支。
func TestResetRestoresOriginalSeed(t *testing.T) {
	seed := map[string][]byte{
		"keep":     []byte(`{"v":"seed-keep"}`),
		"deleted":  []byte(`{"v":"seed-deleted"}`),
		"replaced": []byte(`{"v":"seed-replaced"}`),
	}
	root := NewEnvironment(seed)
	root.Apply([]Change{
		{ID: "deleted", Delete: true},
		{ID: "replaced", Data: []byte(`{"v":"custom"}`)},
		{ID: "added", Data: []byte(`{"v":"added"}`)},
	})

	// 在定制状态下克隆出旁支，旁支继续自己的修改。
	branch := root.Clone()
	branch.Apply([]Change{{ID: "branch-only", Data: []byte(`{"v":"branch"}`)}})

	// 重置 root：回到最初种子，而不是任何中间状态。
	root.Reset()
	assertContent(t, root, "keep", []byte(`{"v":"seed-keep"}`))
	assertContent(t, root, "deleted", []byte(`{"v":"seed-deleted"}`)) // 被删的种子记录重新出现
	assertContent(t, root, "replaced", []byte(`{"v":"seed-replaced"}`))
	assertAbsent(t, root, "added") // 后来新增的记录消失
	assertAbsent(t, root, "branch-only")

	// 旁支不受重置影响。
	assertContent(t, branch, "added", []byte(`{"v":"added"}`))
	assertContent(t, branch, "branch-only", []byte(`{"v":"branch"}`))
	assertAbsent(t, branch, "deleted")

	// 关键：旁支自己被重置时，也回到整条链最初的种子集，而不是克隆时的定制状态。
	branch.Reset()
	assertContent(t, branch, "keep", []byte(`{"v":"seed-keep"}`))
	assertContent(t, branch, "deleted", []byte(`{"v":"seed-deleted"}`))
	assertAbsent(t, branch, "added")
	assertAbsent(t, branch, "branch-only")

	// 重置不改变环境身份：重置后仍可继续定制与克隆。
	root.Apply([]Change{{ID: "keep", Data: []byte(`{"v":"again"}`)}})
	child := root.Clone()
	assertContent(t, child, "keep", []byte(`{"v":"again"}`))
	assertContent(t, root, "deleted", []byte(`{"v":"seed-deleted"}`))
	assertContent(t, child, "deleted", []byte(`{"v":"seed-deleted"}`))
}

// 调用方提交后修改输入字节数组、或修改读取返回的字节数组，都不得影响环境。
func TestByteIsolation(t *testing.T) {
	seedVal := []byte(`{"v":"original"}`)
	seed := map[string][]byte{"rec": seedVal}
	root := NewEnvironment(seed)

	// 提交后篡改种子入参。
	for i := range seedVal {
		seedVal[i] = 'X'
	}
	assertContent(t, root, "rec", []byte(`{"v":"original"}`))

	// 提交后篡改批量定制入参。
	input := []byte(`{"v":"custom"}`)
	root.Apply([]Change{{ID: "rec", Data: input}})
	for i := range input {
		input[i] = 'Y'
	}
	assertContent(t, root, "rec", []byte(`{"v":"custom"}`))

	// 篡改读取返回的数组，不影响环境，也不影响同一环境的再次读取。
	got, _ := root.Read("rec")
	for i := range got {
		got[i] = 'Z'
	}
	assertContent(t, root, "rec", []byte(`{"v":"custom"}`))

	// 克隆后源环境被重置；克隆体内容不因其变化。
	clone := root.Clone()
	root.Reset()
	assertContent(t, clone, "rec", []byte(`{"v":"custom"}`))
	assertContent(t, root, "rec", []byte(`{"v":"original"}`))

	// 已经读出的内容不得因后续定制而变化（此前返回的 got 被调用方写脏，
	// 这里再验证一次读取快照的独立性：重置前的内容来自克隆体）。
	before, _ := clone.Read("rec")
	snapshot := append([]byte(nil), before...)
	clone.Apply([]Change{{ID: "rec", Data: []byte(`{"v":"newer"}`)}})
	assertContent(t, clone, "rec", []byte(`{"v":"newer"}`))
	if !bytes.Equal(before, snapshot) {
		t.Fatalf("previously returned content changed: %q vs %q", before, snapshot)
	}
}

// 随机操作下，与“每个环境独立保存完整副本”的参考模型逐一比对。
func TestAgainstFullCopyModel(t *testing.T) {
	rng := rand.New(rand.NewSource(20261005))

	const keySpace = 60
	keyOf := func(i int) string {
		if i < keySpace {
			return fmt.Sprintf("seed-%02d", i)
		}
		return fmt.Sprintf("extra-%02d", i-keySpace)
	}
	valOf := func(tag, i int) []byte {
		return []byte(fmt.Sprintf(`{"tag":%d,"i":%d,"pad":"%064d"}`, tag, i, i))
	}

	seed := make(map[string][]byte, keySpace)
	for i := 0; i < keySpace; i++ {
		seed[keyOf(i)] = valOf(0, i)
	}

	envs := []*Environment{NewEnvironment(seed)}
	models := []map[string][]byte{cloneModel(seed)}
	seedModel := cloneModel(seed)

	cloneModelOf := func(m map[string][]byte) map[string][]byte { return cloneModel(m) }

	for step := 0; step < 400; step++ {
		op := rng.Intn(4)
		idx := rng.Intn(len(envs))
		switch op {
		case 0: // 克隆
			envs = append(envs, envs[idx].Clone())
			models = append(models, cloneModelOf(models[idx]))
		case 1: // 批量定制（批次内标识不重复）
			n := 1 + rng.Intn(5)
			perm := rng.Perm(keySpace * 2)[:n]
			changes := make([]Change, 0, n)
			for _, p := range perm {
				id := keyOf(p)
				if rng.Intn(3) == 0 {
					changes = append(changes, Change{ID: id, Delete: true})
					delete(models[idx], id)
				} else {
					data := valOf(step+1, p)
					changes = append(changes, Change{ID: id, Data: data})
					models[idx][id] = append([]byte(nil), data...)
				}
			}
			envs[idx].Apply(changes)
			// 批次提交后篡改入参，验证防御性拷贝。
			for _, c := range changes {
				for i := range c.Data {
					c.Data[i] = 0
				}
			}
		case 2: // 重置
			envs[idx].Reset()
			models[idx] = cloneModelOf(seedModel)
		case 3: // 读取校验（含模型中不存在的标识）
			for _, p := range rng.Perm(keySpace * 2)[:10] {
				id := keyOf(p)
				want, ok := models[idx][id]
				got, gotOK := envs[idx].Read(id)
				if gotOK != ok {
					t.Fatalf("step %d env %d key %q: ok=%v want %v", step, idx, id, gotOK, ok)
				}
				if ok && !bytes.Equal(got, want) {
					t.Fatalf("step %d env %d key %q: got %q want %q", step, idx, id, got, want)
				}
			}
		}
	}

	// 终态全量比对：每个环境、每个可能出现的标识。
	for ei, e := range envs {
		for p := 0; p < keySpace*2; p++ {
			id := keyOf(p)
			want, ok := models[ei][id]
			got, gotOK := e.Read(id)
			if gotOK != ok {
				t.Fatalf("final env %d key %q: ok=%v want %v", ei, id, gotOK, ok)
			}
			if ok && !bytes.Equal(got, want) {
				t.Fatalf("final env %d key %q mismatch", ei, id)
			}
		}
	}
	t.Logf("model comparison passed with %d environments", len(envs))
}

func cloneModel(m map[string][]byte) map[string][]byte {
	cp := make(map[string][]byte, len(m))
	for k, v := range m {
		cp[k] = append([]byte(nil), v...)
	}
	return cp
}
