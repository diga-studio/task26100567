package forkdb

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// model 是参考实现：每个环境独立保存完整副本，语义简单直白。
type model struct {
	data map[string][]byte
	seed map[string][]byte
}

func newModel(seed map[string][]byte) *model {
	m := &model{data: map[string][]byte{}, seed: map[string][]byte{}}
	for k, v := range seed {
		m.data[k] = copyBytes(v)
		m.seed[k] = copyBytes(v)
	}
	return m
}

func (m *model) clone() *model {
	c := &model{data: map[string][]byte{}, seed: m.seed}
	for k, v := range m.data {
		c.data[k] = v
	}
	return c
}

func (m *model) apply(muts []Mutation) {
	for _, mu := range muts {
		if mu.Delete {
			delete(m.data, mu.ID)
		} else {
			m.data[mu.ID] = copyBytes(mu.Data)
		}
	}
}

func (m *model) reset() {
	m.data = map[string][]byte{}
	for k, v := range m.seed {
		m.data[k] = v
	}
}

func makeSeed(n int) map[string][]byte {
	seed := make(map[string][]byte, n)
	for i := 0; i < n; i++ {
		seed[fmt.Sprintf("seed-%05d", i)] = []byte(fmt.Sprintf(`{"id":%d,"kind":"seed"}`, i))
	}
	return seed
}

// checkEqual 断言环境内容与参考模型完全一致（逐字节、存在性一致）。
func checkEqual(t *testing.T, e *Env, m *model, allIDs []string) {
	t.Helper()
	for _, id := range allIDs {
		got, ok := e.Get(id)
		want, wok := m.data[id]
		if ok != wok {
			t.Fatalf("id %s: exists=%v, want %v", id, ok, wok)
		}
		if ok && !bytes.Equal(got, want) {
			t.Fatalf("id %s: got %q, want %q", id, got, want)
		}
	}
	count := 0
	hamtForeach(e.root, func(k string, v []byte) {
		count++
		if _, ok := m.data[k]; !ok {
			t.Fatalf("env has extra key %s", k)
		}
	})
	if count != len(m.data) {
		t.Fatalf("env has %d keys, model has %d", count, len(m.data))
	}
}

// TestForkRandomAgainstModel 随机多代分叉，全程与参考模型对比。
// 覆盖：新增、替换、删除（含删种子记录、删新增记录）、克隆、重置、
// 删除不复活、重置回到最初种子且不影响旁支。
func TestForkRandomAgainstModel(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	seed := makeSeed(200)
	envs := []*Env{New(seed)}
	models := []*model{newModel(seed)}

	idPool := []string{}
	for k := range seed {
		idPool = append(idPool, k)
	}
	fresh := 0

	for step := 0; step < 3000; step++ {
		switch rng.Intn(4) {
		case 0: // 克隆
			i := rng.Intn(len(envs))
			envs = append(envs, envs[i].Clone())
			models = append(models, models[i].clone())
		case 1, 2: // 批量定制
			i := rng.Intn(len(envs))
			var muts []Mutation
			for j := 0; j < 1+rng.Intn(8); j++ {
				if rng.Intn(3) == 0 {
					// 新增标识
					id := fmt.Sprintf("new-%05d", fresh)
					fresh++
					idPool = append(idPool, id)
					muts = append(muts, Mutation{ID: id, Data: []byte(fmt.Sprintf(`{"v":%d}`, fresh))})
				} else {
					id := idPool[rng.Intn(len(idPool))]
					if rng.Intn(3) == 0 {
						muts = append(muts, Mutation{ID: id, Delete: true})
					} else {
						muts = append(muts, Mutation{ID: id, Data: []byte(fmt.Sprintf(`{"v":%d,"step":%d}`, j, step))})
					}
				}
			}
			envs[i].Apply(muts)
			models[i].apply(muts)
		case 3: // 重置
			i := rng.Intn(len(envs))
			envs[i].Reset()
			models[i].reset()
		}
		if step%97 == 0 {
			i := rng.Intn(len(envs))
			checkEqual(t, envs[i], models[i], idPool)
		}
	}
	for i := range envs {
		checkEqual(t, envs[i], models[i], idPool)
	}
}

// TestDeleteDoesNotResurrect 删除种子记录后克隆多代，任何后代都读不到它。
func TestDeleteDoesNotResurrect(t *testing.T) {
	seed := makeSeed(10)
	root := New(seed)
	root.Apply([]Mutation{{ID: "seed-00003", Delete: true}})

	cur := root
	for i := 0; i < 50; i++ {
		cur = cur.Clone()
		cur.Apply([]Mutation{{ID: fmt.Sprintf("gen-%d", i), Data: []byte(`1`)}})
		if _, ok := cur.Get("seed-00003"); ok {
			t.Fatalf("gen %d: deleted seed record resurrected", i)
		}
	}
	// 旁支克隆也不受影响
	side := root.Clone()
	if _, ok := side.Get("seed-00003"); ok {
		t.Fatal("side branch sees deleted record")
	}
}

// TestResetReturnsToOriginalSeed 重置回到链最初的种子集，而非克隆时状态；
// 且不影响其他环境。
func TestResetReturnsToOriginalSeed(t *testing.T) {
	seed := makeSeed(10)
	a := New(seed)
	a.Apply([]Mutation{
		{ID: "seed-00001", Data: []byte(`"customized"`)},
		{ID: "seed-00002", Delete: true},
		{ID: "added", Data: []byte(`"new"`)},
	})
	b := a.Clone()
	b.Apply([]Mutation{{ID: "seed-00004", Delete: true}})

	b.Reset()

	// b 回到最初种子：定制消失、删除的种子记录恢复、新增记录消失
	for id, want := range seed {
		got, ok := b.Get(id)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("after reset id %s: got %q ok=%v, want %q", id, got, ok, want)
		}
	}
	if _, ok := b.Get("added"); ok {
		t.Fatal("added record should disappear after reset")
	}
	// a 不受 b 重置影响
	got, _ := a.Get("seed-00001")
	if string(got) != `"customized"` {
		t.Fatalf("a changed by b's reset: %q", got)
	}
	if _, ok := a.Get("seed-00002"); ok {
		t.Fatal("a's deletion lost after b's reset")
	}
	if _, ok := a.Get("added"); !ok {
		t.Fatal("a's added record lost after b's reset")
	}
}

// TestCallerMutationIsolation 调用方修改输入/输出字节数组不得串改环境。
func TestCallerMutationIsolation(t *testing.T) {
	seedData := []byte(`{"k":"seed"}`)
	seed := map[string][]byte{"r1": seedData}
	e := New(seed)

	// 修改种子输入数组
	seedData[2] = 'X'
	got, _ := e.Get("r1")
	if string(got) != `{"k":"seed"}` {
		t.Fatalf("seed input mutation leaked: %q", got)
	}

	// 提交定制后修改输入数组
	in := []byte(`{"k":"v1"}`)
	e.Apply([]Mutation{{ID: "r2", Data: in}})
	in[2] = 'Y'
	got, _ = e.Get("r2")
	if string(got) != `{"k":"v1"}` {
		t.Fatalf("apply input mutation leaked: %q", got)
	}

	// 修改读取返回的数组
	out, _ := e.Get("r2")
	out[2] = 'Z'
	got, _ = e.Get("r2")
	if string(got) != `{"k":"v1"}` {
		t.Fatalf("read output mutation leaked: %q", got)
	}

	// 先前返回的副本不因后续定制而变化
	before, _ := e.Get("r2")
	e.Apply([]Mutation{{ID: "r2", Data: []byte(`{"k":"v2"}`)}})
	if string(before) != `{"k":"v1"}` {
		t.Fatalf("previously returned slice changed: %q", before)
	}
}

// TestCloneIsolation 克隆后双方互不影响，包括继续克隆出的后代。
func TestCloneIsolation(t *testing.T) {
	seed := makeSeed(5)
	a := New(seed)
	b := a.Clone()
	a.Apply([]Mutation{{ID: "seed-00000", Delete: true}})
	b.Apply([]Mutation{{ID: "b-only", Data: []byte(`1`)}})
	c := b.Clone()
	b.Reset()

	if _, ok := b.Get("seed-00000"); !ok {
		t.Fatal("b reset should restore seed, not a's deletion")
	}
	if _, ok := c.Get("b-only"); !ok {
		t.Fatal("c should keep b's state at clone time")
	}
	if _, ok := c.Get("seed-00000"); !ok {
		t.Fatal("c should not see a's deletion")
	}
	if _, ok := a.Get("b-only"); ok {
		t.Fatal("a should not see b's addition")
	}
}
