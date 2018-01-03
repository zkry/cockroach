// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package distsqlrun

import (
	"context"
	"errors"
	"sync"

	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
)

// mergeJoiner performs merge join, it has two input row sources with the same
// ordering on the columns that have equality constraints.
//
// It is guaranteed that the results preserve this ordering.
type mergeJoiner struct {
	joinerBase

	evalCtx *tree.EvalContext

	leftSource, rightSource RowSource
	leftRows, rightRows     []sqlbase.EncDatumRow
	leftIdx, rightIdx       int
	matchedRight            []bool
	matchedRightCount       int

	streamMerger streamMerger
}

var _ Processor = &mergeJoiner{}

func newMergeJoiner(
	flowCtx *FlowCtx,
	spec *MergeJoinerSpec,
	leftSource RowSource,
	rightSource RowSource,
	post *PostProcessSpec,
	output RowReceiver,
) (*mergeJoiner, error) {
	for i, c := range spec.LeftOrdering.Columns {
		if spec.RightOrdering.Columns[i].Direction != c.Direction {
			return nil, errors.New("Unmatched column orderings")
		}
	}

	m := &mergeJoiner{
		leftSource:  leftSource,
		rightSource: rightSource,
	}
	// TODO: Adapt MergeJoiner to new joinerBase constructor.
	err := m.joinerBase.init(flowCtx,
		leftSource.OutputTypes(), rightSource.OutputTypes(),
		spec.Type, spec.OnExpr, nil, nil, 0, post, output)
	if err != nil {
		return nil, err
	}

	m.streamMerger, err = makeStreamMerger(
		leftSource,
		convertToColumnOrdering(spec.LeftOrdering),
		rightSource,
		convertToColumnOrdering(spec.RightOrdering),
		spec.NullEquality,
	)
	if err != nil {
		return nil, err
	}

	return m, nil
}

// Run is part of the processor interface.
func (m *mergeJoiner) Run(wg *sync.WaitGroup) {
	if wg != nil {
		defer wg.Done()
	}

	ctx := log.WithLogTag(m.flowCtx.Ctx, "MergeJoiner", nil)
	ctx, span := processorSpan(ctx, "merge joiner")
	defer tracing.FinishSpan(span)
	log.VEventf(ctx, 2, "starting merge joiner run")

	cancelChecker := sqlbase.NewCancelChecker(ctx)
	m.evalCtx = m.flowCtx.NewEvalCtx()

	for {
		moreBatches, err := m.outputBatch(ctx, cancelChecker)
		if err != nil || !moreBatches {
			DrainAndClose(
				ctx, m.out.output, err, m.leftSource, m.rightSource)
			break
		}
	}
	log.VEventf(ctx, 2, "exiting merge joiner run")
}

// outputBatch outputs all the rows corresponding to a streamMerger batch (the
// cross-product of two groups of matching rows).
//
// Returns true if more batches are available and needed. If false is returned,
// the caller should drain the inputs (as the termination condition might have
// been dictated by the consumer saying that no more rows are needed) and close
// the output.
func (m *mergeJoiner) outputBatch(
	ctx context.Context, cancelChecker *sqlbase.CancelChecker,
) (bool, error) {
	for {
		var meta *ProducerMetadata
		m.leftRows, m.rightRows, meta = m.streamMerger.NextBatch(m.evalCtx)
		if meta != nil {
			if meta.Err != nil {
				return false, meta.Err
			}
			_ = m.out.output.Push(nil /* row */, *meta)
			continue
		}
		if m.leftRows == nil && m.rightRows == nil {
			return false, nil
		}

		m.matchedRight = nil
		if m.joinType == fullOuter || m.joinType == rightOuter {
			m.matchedRight = make([]bool, len(m.rightRows))
		}
		break
	}

	for {
		if err := cancelChecker.Check(); err != nil {
			return false, err
		}
		batchDone, err := m.outputOneRow(ctx)
		if err != nil {
			return false, err
		}
		if !batchDone {
			return true, nil
		}
	}
}

func (m *mergeJoiner) outputOneRow(ctx context.Context) (bool, error) {
	for m.leftIdx < len(m.leftRows) {
		// Main join loop.
		lrow := m.leftRows[m.leftIdx]
		for m.rightIdx < len(m.rightRows) {
			ridx := m.rightIdx
			m.rightIdx++
			renderedRow, err := m.render(lrow, m.rightRows[ridx])
			if err != nil {
				return false, err
			}
			if renderedRow != nil {
				m.matchedRightCount++
				if m.matchedRight != nil {
					m.matchedRight[ridx] = true
				}
				consumerStatus, err := m.out.EmitRow(ctx, renderedRow)
				if err != nil || consumerStatus != NeedMoreRows {
					return false, err
				}
				return true, nil
			}
		}

		m.rightIdx = 0
		m.leftIdx++

		// We've exhausted the right side batch.
		if m.matchedRightCount == 0 {
			// Maybe output the left side row if no right side rows were matched.
			needMoreRows, err := m.maybeEmitUnmatchedRow(ctx, lrow, leftSide)
			if !needMoreRows || err != nil {
				return false, err
			}
			return true, nil
		}

		m.matchedRightCount = 0
	}

	if m.matchedRight != nil {
		// No-more rows on the left side, maybe output unmatched rows from the
		// right side.
		for m.rightIdx < len(m.rightRows) {
			ridx := m.rightIdx
			m.rightIdx++
			if m.matchedRight[ridx] {
				continue
			}
			needMoreRows, err := m.maybeEmitUnmatchedRow(ctx, m.rightRows[ridx], rightSide)
			if !needMoreRows || err != nil {
				return false, err
			}
			return true, nil
		}

		m.matchedRight = nil
	}

	m.leftIdx, m.rightIdx = 0, 0
	m.leftRows, m.rightRows = nil, nil
	return false, nil
}
