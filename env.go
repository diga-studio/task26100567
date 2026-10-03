package forkdb

// Mutation 描述一条批量定制操作：Delete 为 true 表示删除该标识，
// 否则将标识的内容整条设置为 Data（新增或替换）。
type Mutation struct {
	ID     string
	Data   []byte
	Delete bool
}

// Env 是一个演示数据环境。请用 New 创建、Clone 分叉。
// 所有方法按单线程顺序调用设计，不提供并发安全。
type Env struct {
	root node // 当前内容的 HAMT 根
	seed node // 这条分叉链最初创建时的种子集根，重置用
}

// New 从上层提供的种子集创建环境。种子内容会被逐字节复制，
// 调用方之后修改输入不影响环境。
func New(seed map[string][]byte) *Env {
	var root node
	for id, data := range seed {
		root = hamtSet(root, hashKey(id), id, copyBytes(data), 0)
	}
	return &Env{root: root, seed: root}
}

// Get 按记录标识读取内容。返回内容的独立副本，调用方修改返回值
// 不影响环境；第二个返回值表示该标识当前是否存在。
func (e *Env) Get(id string) ([]byte, bool) {
	return hamtGet(e.root, hashKey(id), id)
}

// Apply 批量定制：新增、整条替换、删除。批次内标识不重复。
// 输入字节数组会被复制，调用方提交后修改输入不影响环境。
// 已返回给其他调用方的内容副本也不受影响。
func (e *Env) Apply(muts []Mutation) {
	root := e.root
	for _, m := range muts {
		hash := hashKey(m.ID)
		if m.Delete {
			root = hamtDel(root, hash, m.ID, 0)
		} else {
			root = hamtSet(root, hash, m.ID, copyBytes(m.Data), 0)
		}
	}
	e.root = root
}

// Clone 返回源环境在此时的完整逻辑副本，记录标识保持不变。
// 之后双方的定制、重置、继续克隆互不影响：内部结构不可变且共享，
// 克隆是 O(1) 的。
func (e *Env) Clone() *Env {
	return &Env{root: e.root, seed: e.seed}
}

// Reset 将环境恢复为这条分叉链最初创建时的种子集，而不是克隆时
// 的定制状态。不改变环境身份，也不影响其他环境。
func (e *Env) Reset() {
	e.root = e.seed
}

func copyBytes(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
