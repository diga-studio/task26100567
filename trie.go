package forkdb

import "math/bits"

// 持久化 HAMT（Hash Array Mapped Trie）。
// 节点不可变，插入/删除通过路径复制生成新根，旧根及其结构全部被共享，
// 因此克隆 O(1)，单次修改 O(树高)，历史版本零拷贝保留。

const (
	shiftBits  = 5
	branchSize = 1 << shiftBits // 32
	indexMask  = branchSize - 1
	maxShift   = 64 // 哈希位数耗尽后进入冲突节点
)

type node interface{ trieNode() }

// bitmapNode 用位图标记 32 个槽位中哪些有子节点，children 按下标密集存储。
type bitmapNode struct {
	bitmap   uint32
	children []node
}

type leafNode struct {
	hash uint64
	key  string
	val  []byte
}

// collisionNode 保存 64 位哈希完全相同的不同键，概率极低，仅作兜底。
type collisionNode struct {
	hash   uint64
	leaves []leafNode
}

func (*bitmapNode) trieNode()    {}
func (*leafNode) trieNode()      {}
func (*collisionNode) trieNode() {}

// hashKey 是内联的 FNV-1a 64，避免 hash 包的接口开销。
func hashKey(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

func trieGet(n node, hash uint64, key string, shift uint) ([]byte, bool) {
	for {
		switch t := n.(type) {
		case *bitmapNode:
			bit := uint32(1) << ((hash >> shift) & indexMask)
			if t.bitmap&bit == 0 {
				return nil, false
			}
			n = t.children[bits.OnesCount32(t.bitmap&(bit-1))]
			shift += shiftBits
		case *leafNode:
			if t.key == key {
				return t.val, true
			}
			return nil, false
		case *collisionNode:
			for i := range t.leaves {
				if t.leaves[i].key == key {
					return t.leaves[i].val, true
				}
			}
			return nil, false
		default:
			return nil, false
		}
	}
}

func trieInsert(n node, hash uint64, key string, val []byte, shift uint) node {
	switch t := n.(type) {
	case nil:
		return &leafNode{hash: hash, key: key, val: val}
	case *bitmapNode:
		bit := uint32(1) << ((hash >> shift) & indexMask)
		pos := bits.OnesCount32(t.bitmap & (bit - 1))
		if t.bitmap&bit != 0 {
			child := trieInsert(t.children[pos], hash, key, val, shift+shiftBits)
			children := make([]node, len(t.children))
			copy(children, t.children)
			children[pos] = child
			return &bitmapNode{bitmap: t.bitmap, children: children}
		}
		children := make([]node, len(t.children)+1)
		copy(children, t.children[:pos])
		children[pos] = &leafNode{hash: hash, key: key, val: val}
		copy(children[pos+1:], t.children[pos:])
		return &bitmapNode{bitmap: t.bitmap | bit, children: children}
	case *leafNode:
		if t.key == key {
			return &leafNode{hash: hash, key: key, val: val}
		}
		return splitAt(t, t.hash, &leafNode{hash: hash, key: key, val: val}, shift)
	case *collisionNode:
		if t.hash == hash {
			leaves := make([]leafNode, len(t.leaves))
			copy(leaves, t.leaves)
			for i := range leaves {
				if leaves[i].key == key {
					leaves[i].val = val
					return &collisionNode{hash: hash, leaves: leaves}
				}
			}
			return &collisionNode{hash: hash, leaves: append(leaves, leafNode{hash: hash, key: key, val: val})}
		}
		return splitAt(t, t.hash, &leafNode{hash: hash, key: key, val: val}, shift)
	}
	panic("unreachable")
}

// splitAt 把已有节点（叶或冲突节点）与新叶按哈希逐层分开，直到槽位不同。
func splitAt(existing node, existingHash uint64, leaf *leafNode, shift uint) node {
	if shift >= maxShift {
		if c, ok := existing.(*collisionNode); ok && c.hash == leaf.hash {
			leaves := make([]leafNode, len(c.leaves)+1)
			copy(leaves, c.leaves)
			leaves[len(c.leaves)] = *leaf
			return &collisionNode{hash: c.hash, leaves: leaves}
		}
		if l, ok := existing.(*leafNode); ok && l.hash == leaf.hash {
			return &collisionNode{hash: leaf.hash, leaves: []leafNode{*l, *leaf}}
		}
	}
	ei := (existingHash >> shift) & indexMask
	li := (leaf.hash >> shift) & indexMask
	if ei == li {
		child := splitAt(existing, existingHash, leaf, shift+shiftBits)
		return &bitmapNode{bitmap: uint32(1) << ei, children: []node{child}}
	}
	bm := (uint32(1) << ei) | (uint32(1) << li)
	if ei < li {
		return &bitmapNode{bitmap: bm, children: []node{existing, leaf}}
	}
	return &bitmapNode{bitmap: bm, children: []node{leaf, existing}}
}

// trieDelete 返回删除后的新子树；键不存在时原样返回 n（调用方可据此判断无变化）。
// 返回 nil 表示子树已空。
func trieDelete(n node, hash uint64, key string, shift uint) node {
	switch t := n.(type) {
	case nil:
		return nil
	case *bitmapNode:
		bit := uint32(1) << ((hash >> shift) & indexMask)
		if t.bitmap&bit == 0 {
			return n
		}
		pos := bits.OnesCount32(t.bitmap & (bit - 1))
		child := trieDelete(t.children[pos], hash, key, shift+shiftBits)
		if child == t.children[pos] {
			return n
		}
		if child == nil {
			nb := t.bitmap &^ bit
			if nb == 0 {
				return nil
			}
			children := make([]node, len(t.children)-1)
			copy(children, t.children[:pos])
			copy(children[pos:], t.children[pos+1:])
			// 只剩一个非位图子节点时向上收缩，保持树高最小。
			if len(children) == 1 {
				if _, isBitmap := children[0].(*bitmapNode); !isBitmap {
					return children[0]
				}
			}
			return &bitmapNode{bitmap: nb, children: children}
		}
		children := make([]node, len(t.children))
		copy(children, t.children)
		children[pos] = child
		return &bitmapNode{bitmap: t.bitmap, children: children}
	case *leafNode:
		if t.key == key {
			return nil
		}
		return n
	case *collisionNode:
		for i := range t.leaves {
			if t.leaves[i].key == key {
				if len(t.leaves) == 2 {
					other := t.leaves[1-i]
					return &other
				}
				leaves := make([]leafNode, len(t.leaves)-1)
				copy(leaves, t.leaves[:i])
				copy(leaves[i:], t.leaves[i+1:])
				return &collisionNode{hash: t.hash, leaves: leaves}
			}
		}
		return n
	}
	panic("unreachable")
}
