package plist

import (
	"errors"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// archiveShape is one keyed-archive shape, built as the plain value Decode returns.
type archiveShape struct {
	name  string
	build func() map[string]any
}

// archiveBuilder assembles $objects; the first object added has index 1 ($objects[0] is
// "$null", put in front by archive()).
type archiveBuilder struct{ objs []any }

func (b *archiveBuilder) add(o any) uint64 {
	b.objs = append(b.objs, o)
	return uint64(len(b.objs))
}

func (b *archiveBuilder) archive(root uint64) map[string]any { return archive(root, b.objs...) }

// listObj adds an array-like object of the class with the given element references.
func (b *archiveBuilder) listObj(class uint64, refs []any) uint64 {
	return b.add(map[string]any{"$class": UID(class), "NS.objects": refs})
}

// rootList builds an archive whose root is one list object of the class named, holding the
// elements that fill returns (each added through the builder); it returns the references.
func rootList(class string, n int, elem func(b *archiveBuilder, cls uint64, i int) uint64) map[string]any {
	b := &archiveBuilder{}
	root := b.add(nil) // replaced below
	cls := b.add(classRec(class))
	refs := make([]any, n)
	for i := range refs {
		refs[i] = UID(elem(b, cls, i))
	}
	b.objs[root-1] = map[string]any{"$class": UID(cls), "NS.objects": refs}
	return b.archive(root)
}

func archiveShapes() []archiveShape {
	return []archiveShape{
		{"array of 100000 strings", func() map[string]any {
			return rootList("NSArray", 100000, func(b *archiveBuilder, _ uint64, i int) uint64 {
				return b.add("s" + strconv.Itoa(i))
			})
		}},
		{"10000 two-key dictionaries", func() map[string]any {
			b := &archiveBuilder{}
			root := b.add(nil)
			arrCls := b.add(classRec("NSArray"))
			dictCls := b.add(classRec("NSDictionary"))
			k1, k2 := b.add("alpha"), b.add("beta")
			refs := make([]any, 10000)
			for i := range refs {
				v1, v2 := b.add(int64(i)), b.add("v"+strconv.Itoa(i))
				refs[i] = UID(b.add(map[string]any{
					"$class": UID(dictCls), "NS.keys": []any{UID(k1), UID(k2)}, "NS.objects": []any{UID(v1), UID(v2)},
				}))
			}
			b.objs[root-1] = map[string]any{"$class": UID(arrCls), "NS.objects": refs}
			return b.archive(root)
		}},
		{"one dictionary of 50000 distinct keys", func() map[string]any {
			b := &archiveBuilder{}
			root := b.add(nil)
			cls := b.add(classRec("NSDictionary"))
			keys, vals := make([]any, 50000), make([]any, 50000)
			for i := range keys {
				keys[i], vals[i] = UID(b.add("key"+strconv.Itoa(i))), UID(b.add(int64(i)))
			}
			b.objs[root-1] = map[string]any{"$class": UID(cls), "NS.keys": keys, "NS.objects": vals}
			return b.archive(root)
		}},
		{"duplicate-key bomb", func() map[string]any { return bigDupDictArchive(100000) }},
		{"set of 100000", func() map[string]any {
			return rootList("NSSet", 100000, func(b *archiveBuilder, _ uint64, i int) uint64 {
				return b.add("e" + strconv.Itoa(i))
			})
		}},
		{"array referencing one object 200000 times", func() map[string]any {
			b := &archiveBuilder{}
			root := b.add(nil)
			cls := b.add(classRec("NSArray"))
			shared := b.add("shared")
			refs := make([]any, 200000)
			for i := range refs {
				refs[i] = UID(shared)
			}
			b.objs[root-1] = map[string]any{"$class": UID(cls), "NS.objects": refs}
			return b.archive(root)
		}},
		{"1000 strings of 4 KiB", func() map[string]any {
			return rootList("NSArray", 1000, func(b *archiveBuilder, _ uint64, i int) uint64 {
				return b.add(strings.Repeat("s", 4096) + strconv.Itoa(i))
			})
		}},
		{"1000 NSData of 4 KiB", func() map[string]any {
			data := uint64(0)
			return rootList("NSArray", 1000, func(b *archiveBuilder, _ uint64, _ int) uint64 {
				if data == 0 {
					data = b.add(classRec("NSData"))
				}
				return b.add(map[string]any{"$class": UID(data), "NS.data": make([]byte, 4096)})
			})
		}},
		{"50000 NSDate", func() map[string]any {
			date := uint64(0)
			return rootList("NSArray", 50000, func(b *archiveBuilder, _ uint64, i int) uint64 {
				if date == 0 {
					date = b.add(classRec("NSDate"))
				}
				return b.add(map[string]any{"$class": UID(date), "NS.time": float64(i)})
			})
		}},
		{"50000 objects of an unknown class with 6 fields", func() map[string]any {
			unk := uint64(0)
			return rootList("NSArray", 50000, func(b *archiveBuilder, _ uint64, i int) uint64 {
				if unk == 0 {
					unk = b.add(classRec("MyThing"))
				}
				return b.add(map[string]any{
					"$class": UID(unk), "a": int64(i), "b": "text", "c": true, "d": 1.5,
					"e": []any{int64(1), int64(2)}, "f": map[string]any{"x": int64(i)},
				})
			})
		}},
		{"nested arrays 56 deep, 1000 times", func() map[string]any {
			return rootList("NSArray", 1000, func(b *archiveBuilder, cls uint64, i int) uint64 {
				leaf := b.add(int64(i))
				for range 55 {
					leaf = b.listObj(cls, []any{UID(leaf)})
				}
				return leaf
			})
		}},
		{"50000 small objects, 10000 of them dictionaries", func() map[string]any {
			dict, key := uint64(0), uint64(0)
			return rootList("NSArray", 50000, func(b *archiveBuilder, _ uint64, i int) uint64 {
				if dict == 0 {
					dict, key = b.add(classRec("NSDictionary")), b.add("k")
				}
				if i%5 != 0 {
					return b.add("o" + strconv.Itoa(i))
				}
				v := b.add(int64(i))
				return b.add(map[string]any{"$class": UID(dict), "NS.keys": []any{UID(key)}, "NS.objects": []any{UID(v)}})
			})
		}},
	}
}

// TestUnarchiveChargeCoversMeasuredAllocation is TestChargeCoversMeasuredAllocation for
// Unarchive: on every archive shape the budget charge is at least the bytes the call allocates.
func TestUnarchiveChargeCoversMeasuredAllocation(t *testing.T) {
	for _, s := range archiveShapes() {
		t.Run(s.name, func(t *testing.T) {
			a := s.build()
			rec := &recBudget{}
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, _, err := UnarchiveReport(a, rec)
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatalf("UnarchiveReport: %v", err)
			}
			allocated := int64(after.TotalAlloc - before.TotalAlloc)
			payload, items := archiveCostOf(a)
			need := float64(allocated-payload-items*chargePerItemUA) / float64(rec.calls)
			t.Logf("CHARGE shape=%q objects=%d payload=%d items=%d charged=%d allocated=%d ratio=%.3f needPerObject=%.1f",
				s.name, rec.calls, payload, items, rec.sum, allocated, float64(allocated)/float64(rec.sum), need)
			if rec.sum < allocated {
				t.Errorf("under-charge: charged %d, allocated %d", rec.sum, allocated)
			}
			// A full 25 percent margin on the total: allocated/charged <= 0.80 (ruling V62).
			if float64(allocated) > 0.80*float64(rec.sum) {
				t.Errorf("margin below 25 percent: allocated %d is %.3f of the charge %d", allocated, float64(allocated)/float64(rec.sum), rec.sum)
			}
		})
	}
}

// archiveCostOf returns the payload bytes and the items the unarchiver derives for the objects
// of an archive that it resolves (class records are looked up, not resolved, and are skipped),
// so a shape's measurement can be split into a per-object and a per-item part.
func archiveCostOf(a map[string]any) (payload, items int64) {
	for i, o := range a["$objects"].([]any) {
		if i == 0 {
			continue
		}
		if n, ok := primitive(o); ok {
			payload += int64(n)
			continue
		}
		fields, ok := o.(map[string]any)
		if !ok || fields["$classname"] != nil {
			continue
		}
		var c fieldCost
		c.items = int64(len(fields))
		for k, e := range fields {
			c.payload += int64(len(k))
			c.add(e, 0)
		}
		if l, ok := fields["NS.keys"].([]any); ok {
			c.items += int64(len(l))
		}
		payload, items = payload+c.payload, items+c.items
	}
	return payload, items
}

// TestUnarchiveBudgetBoundarySizedFromTheConstants: the budget that an archive needs is
// computed from the constants (objects, payload and items), an archive one byte over that
// budget is refused with ErrNoBudget and returns nothing, and one at it decodes.
func TestUnarchiveBudgetBoundarySizedFromTheConstants(t *testing.T) {
	a := rootList("NSArray", 1000, func(b *archiveBuilder, _ uint64, i int) uint64 {
		return b.add("s" + strconv.Itoa(i))
	})
	payload, items := archiveCostOf(a)
	objects := int64(1000 + 1) // the root and 1000 strings; the class record is looked up, not resolved
	need := objects*chargePerObjUA + payload + items*chargePerItemUA

	out, _, err := UnarchiveReport(a, newView(need))
	if err != nil || out == nil {
		t.Fatalf("a budget of exactly %d: %v", need, err)
	}
	view := newView(need - 1)
	out, _, err = UnarchiveReport(a, view)
	if !errors.Is(err, ErrNoBudget) || out != nil {
		t.Fatalf("a budget one byte short: value %v, err %v; want nil and ErrNoBudget", out != nil, err)
	}
	if view.Used() != 0 {
		t.Fatalf("the refused archive keeps %d bytes charged", view.Used())
	}
}
