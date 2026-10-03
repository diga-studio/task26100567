package forkdb

// Change 描述一条批量定制操作：Delete 为 true 表示删除该标识，
// 否则用 Data 整条新增或替换。同一批次内标识不重复。
type Change struct {
	ID     string
	Data   []byte
	Delete bool
}

// Environment 是分叉链上的一个环境。seed 指向整条链最初创建时的种子集
// （全链共享、永不变更），root 指向当前逻辑内容。两者都是不可变持久结构，
// 因此 Clone 是 O(1) 的，历史环境无需拷贝即可全部保留。
//
// 仅支持单线程顺序调用，不做并发同步。
type Environment struct {
	seed node
	root node
}

// NewEnvironment 用上层提供的种子集创建分叉链上的第一个环境。
// 种子内容会被逐字节拷入，调用方随后修改入参不影响环境。
func NewEnvironment(seed map[string][]byte) *Environment {
	var root node
	for k, v := range seed {
		root = trieInsert(root, hashKey(k), k, cloneBytes(v), 0)
	}
	return &Environment{seed: root, root: root}
}

// Read 按标识读取记录内容。返回的是独立副本，调用方修改不影响环境；
// 记录不存在（含被删除）时 ok 为 false。
func (e *Environment) Read(id string) (data []byte, ok bool) {
	v, ok := trieGet(e.root, hashKey(id), id, 0)
	if !ok {
		return nil, false
	}
	return cloneBytes(v), true
}

// Apply 批量定制：按顺序应用新增、整条替换和删除。
// 输入的 Data 会被拷入，提交后调用方修改入参不影响环境。
// 只修改当前环境，其祖先、克隆源及其他分支均不受影响。
func (e *Environment) Apply(changes []Change) {
	root := e.root
	for _, c := range changes {
		h := hashKey(c.ID)
		if c.Delete {
			root = trieDelete(root, h, c.ID, 0)
		} else {
			root = trieInsert(root, h, c.ID, cloneBytes(c.Data), 0)
		}
	}
	e.root = root
}

// Clone 返回当前环境在此刻的完整逻辑副本（含新增、替换、删除的结果），
// 记录标识保持不变。此后双方的定制、重置、再克隆互不影响。
func (e *Environment) Clone() *Environment {
	return &Environment{seed: e.seed, root: e.root}
}

// Reset 把当前环境恢复为这条分叉链最初创建时的种子集：
// 种子记录恢复原内容，后来新增的记录消失，被删除的种子记录重新出现。
// 不改变环境身份，不影响其他环境。
func (e *Environment) Reset() {
	e.root = e.seed
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}
