package forkdb

import (
	"hash/fnv"
	"math/bits"
)

// node 是不可变 HAMT（Hash Array Mapped Trie）的节点。
// 所有修改操作都返回新节点，旧节点保持不变，从而支持 O(1) 克隆与结构共享。
type node interface{}

// bitmapNode 是内部节点：bitmap 标记哪些槽位有子节点，children 按位序紧凑排列。
type bitmapNode struct {
	bitmap   uint32
	children []node
}

// leaf 保存一条记录。key 用于处理哈希碰撞，val 为记录内容的独立副本。
type leaf struct {
	hash uint64
	key  string
	val  []byte
}

// collision 保存哈希完全相同但 key 不同的一组记录（极罕见）。
type collision struct {
	hash   uint64
	leaves []leaf
}

const hamtBits = 5    // 每层消耗 5 位哈希
const hamtMask = 0x1f // 32 路分支

func hashKey(key string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(key))
	return h.Sum64()
}

func slotOf(hash uint64, depth uint) uint32 {
	return uint32(hash >> (depth * hamtBits) & hamtMask)
}

// hamtGet 查找 key，返回值的独立副本与是否存在。
func hamtGet(n node, hash uint64, key string) ([]byte, bool) {
	for depth := uint(0); ; depth++ {
		switch t := n.(type) {
		case *bitmapNode:
			bit := uint32(1) << slotOf(hash, depth)
			if t.bitmap&bit == 0 {
				return nil, false
			}
			n = t.children[bits.OnesCount32(t.bitmap&(bit-1))]
		case *leaf:
			if t.hash == hash && t.key == key {
				out := make([]byte, len(t.val))
				copy(out, t.val)
				return out, true
			}
			return nil, false
		case *collision:
			for i := range t.leaves {
				if t.leaves[i].key == key {
					out := make([]byte, len(t.leaves[i].val))
					copy(out, t.leaves[i].val)
					return out, true
				}
			}
			return nil, false
		default:
			return nil, false
		}
	}
}

// hamtSet 插入或替换 key，返回新子树根。val 必须已是环境私有的副本。
func hamtSet(n node, hash uint64, key string, val []byte, depth uint) node {
	if n == nil {
		return &leaf{hash: hash, key: key, val: val}
	}
	switch t := n.(type) {
	case *bitmapNode:
		bit := uint32(1) << slotOf(hash, depth)
		idx := bits.OnesCount32(t.bitmap & (bit - 1))
		if t.bitmap&bit == 0 {
			children := make([]node, len(t.children)+1)
			copy(children, t.children[:idx])
			copy(children[idx+1:], t.children[idx:])
			children[idx] = &leaf{hash: hash, key: key, val: val}
			return &bitmapNode{bitmap: t.bitmap | bit, children: children}
		}
		children := make([]node, len(t.children))
		copy(children, t.children)
		children[idx] = hamtSet(children[idx], hash, key, val, depth+1)
		return &bitmapNode{bitmap: t.bitmap, children: children}
	case *leaf:
		if t.hash == hash && t.key == key {
			return &leaf{hash: hash, key: key, val: val}
		}
		if t.hash == hash {
			return &collision{hash: hash, leaves: []leaf{*t, {hash: hash, key: key, val: val}}}
		}
		return mergeNodes(t.hash, t, hash, &leaf{hash: hash, key: key, val: val}, depth)
	case *collision:
		if t.hash == hash {
			leaves := make([]leaf, len(t.leaves))
			copy(leaves, t.leaves)
			for i := range leaves {
				if leaves[i].key == key {
					leaves[i].val = val
					return &collision{hash: hash, leaves: leaves}
				}
			}
			return &collision{hash: hash, leaves: append(leaves, leaf{hash: hash, key: key, val: val})}
		}
		return mergeNodes(t.hash, t, hash, &leaf{hash: hash, key: key, val: val}, depth)
	}
	return n
}

// mergeNodes 把两个哈希不同的子树按位分层展开成内部节点。
func mergeNodes(aHash uint64, a node, bHash uint64, b node, depth uint) node {
	sa := slotOf(aHash, depth)
	sb := slotOf(bHash, depth)
	if sa == sb {
		child := mergeNodes(aHash, a, bHash, b, depth+1)
		return &bitmapNode{bitmap: 1 << sa, children: []node{child}}
	}
	if sa < sb {
		return &bitmapNode{bitmap: 1<<sa | 1<<sb, children: []node{a, b}}
	}
	return &bitmapNode{bitmap: 1<<sa | 1<<sb, children: []node{b, a}}
}

// hamtDel 删除 key，返回新子树根（可能为 nil）。
func hamtDel(n node, hash uint64, key string, depth uint) node {
	switch t := n.(type) {
	case *bitmapNode:
		bit := uint32(1) << slotOf(hash, depth)
		if t.bitmap&bit == 0 {
			return n
		}
		idx := bits.OnesCount32(t.bitmap & (bit - 1))
		child := hamtDel(t.children[idx], hash, key, depth+1)
		if child == t.children[idx] {
			return n
		}
		if child == nil {
			if len(t.children) == 1 {
				return nil
			}
			// 压缩：只剩一个非内部子节点时直接上提，避免空内部节点累积
			if len(t.children) == 2 {
				other := t.children[1-idx]
				if _, ok := other.(*bitmapNode); !ok {
					return other
				}
			}
			children := make([]node, len(t.children)-1)
			copy(children, t.children[:idx])
			copy(children[idx:], t.children[idx+1:])
			return &bitmapNode{bitmap: t.bitmap &^ bit, children: children}
		}
		children := make([]node, len(t.children))
		copy(children, t.children)
		children[idx] = child
		return &bitmapNode{bitmap: t.bitmap, children: children}
	case *leaf:
		if t.hash == hash && t.key == key {
			return nil
		}
		return n
	case *collision:
		for i := range t.leaves {
			if t.leaves[i].key == key {
				if len(t.leaves) == 2 {
					other := t.leaves[1-i]
					return &leaf{hash: other.hash, key: other.key, val: other.val}
				}
				leaves := make([]leaf, len(t.leaves)-1)
				copy(leaves, t.leaves[:i])
				copy(leaves[i:], t.leaves[i+1:])
				return &collision{hash: t.hash, leaves: leaves}
			}
		}
		return n
	}
	return n
}

// hamtForeach 遍历所有记录（用于测试）。
func hamtForeach(n node, fn func(key string, val []byte)) {
	switch t := n.(type) {
	case *bitmapNode:
		for _, c := range t.children {
			hamtForeach(c, fn)
		}
	case *leaf:
		fn(t.key, t.val)
	case *collision:
		for i := range t.leaves {
			fn(t.leaves[i].key, t.leaves[i].val)
		}
	}
}
