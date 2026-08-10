// Package query provides distributed physical-plan span derivation and a
// bounded, cancellable processor runtime.
package query

import (
	"bytes"
	"fmt"

	"pebbledb/catalog"
	"pebbledb/codec"
	"pebbledb/distributed/ranges"
	"pebbledb/sql/plan"
	"pebbledb/storage/indexstore"
	"pebbledb/storage/mvcc"
	"pebbledb/storage/rowstore"
)

// ScanSpan is one range-local fragment derived from a physical scan.
type ScanSpan struct {
	RangeID     uint64
	ReplicaID   uint64
	Generation  uint64
	Table       catalog.TableDescriptor
	Start       []byte
	End         []byte
	LogicalFrom []byte
	LogicalTo   []byte
}

// DeriveScanSpans turns every physical scan into intersections with the current
// range map. Primary and secondary equality access paths derive narrow spans.
func DeriveScanSpans(physical plan.PhysicalPlan, descriptors []ranges.Descriptor) ([]ScanSpan, error) {
	if physical.Root == nil {
		return nil, fmt.Errorf("distributed query: physical plan has no root")
	}
	var result []ScanSpan
	var visit func(*plan.PhysicalNode) error
	visit = func(node *plan.PhysicalNode) error {
		if node == nil {
			return nil
		}
		if node.Kind == plan.ScanKind {
			logicalStart, logicalEnd, err := logicalScanSpan(node)
			if err != nil {
				return err
			}
			physicalStart, physicalEnd := mvcc.PhysicalSpan(logicalStart, logicalEnd)
			for _, descriptor := range descriptors {
				if !spanIntersects(physicalStart, physicalEnd, descriptor.Start, descriptor.End) {
					continue
				}
				result = append(result, ScanSpan{
					RangeID: descriptor.ID, ReplicaID: descriptor.ReplicaID, Generation: descriptor.Generation,
					Table: node.Table, Start: maxBound(physicalStart, descriptor.Start), End: minBound(physicalEnd, descriptor.End),
					LogicalFrom: clone(logicalStart), LogicalTo: clone(logicalEnd),
				})
			}
		}
		if err := visit(node.Input); err != nil {
			return err
		}
		return visit(node.Right)
	}
	if err := visit(physical.Root); err != nil {
		return nil, err
	}
	return result, nil
}

func logicalScanSpan(node *plan.PhysicalNode) ([]byte, []byte, error) {
	switch node.Access {
	case plan.PrimaryKeyLookup:
		key, err := rowstore.Key(node.Table, node.LookupValues)
		if err != nil {
			return nil, nil, err
		}
		return key, codec.PrefixEnd(key), nil
	case plan.SecondaryIndexScan:
		if node.Index == nil {
			return nil, nil, fmt.Errorf("distributed query: secondary scan has no index")
		}
		values, err := codec.EncodeKey(node.LookupValues...)
		if err != nil {
			return nil, nil, err
		}
		prefix := append(indexstore.IndexPrefix(node.Table.ID, node.Index.ID), values...)
		return prefix, codec.PrefixEnd(prefix), nil
	default:
		prefix := rowstore.TablePrefix(node.Table.ID)
		return prefix, codec.PrefixEnd(prefix), nil
	}
}

func spanIntersects(leftStart, leftEnd, rightStart, rightEnd []byte) bool {
	return (len(leftEnd) == 0 || len(rightStart) == 0 || bytes.Compare(rightStart, leftEnd) < 0) &&
		(len(rightEnd) == 0 || len(leftStart) == 0 || bytes.Compare(leftStart, rightEnd) < 0)
}

func maxBound(left, right []byte) []byte {
	if len(left) == 0 || (len(right) > 0 && bytes.Compare(right, left) > 0) {
		return clone(right)
	}
	return clone(left)
}

func minBound(left, right []byte) []byte {
	if len(left) == 0 {
		return clone(right)
	}
	if len(right) == 0 || bytes.Compare(left, right) < 0 {
		return clone(left)
	}
	return clone(right)
}

func clone(value []byte) []byte { return append([]byte(nil), value...) }
