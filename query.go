package main

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"fmt"
	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
	"github.com/pkg/errors"
	"math/rand"
)

type shapeType string
type operationType string

const (
	// cellShape is the bounds of an s2 cell. This is the best case for s2.
	cellShape shapeType = "cell"
	// capShape is a point and radius.
	capShape shapeType = "cap"
	// rectShape is the rectangle definited by two opposite corners.
	rectShape shapeType = "rect"
	// containsOperation retrieves indexed bounding boxes entirely contained by
	// this shape's bounding box.
	containsOperation operationType = "contains"
	// containsOperation retrieves indexed bounding boxes entirely containing
	// this shape's bounding box.
	containingOperation operationType = "containing"
	// intersectsOperation retrieves indexed bounding boxes that intersect
	// this shape's bounding box. This is the union of contains and containing.
	intersectsOperation operationType = "intersects"
)

func arcFromLevel(level int) float64 {
	return s2.AvgAngleSpanMetric.Value(level)
}

func metersFromLevel(level int) float64 {
	return float64(earthRadiusMeters) * arcFromLevel(level)
}

type queryReader struct {
	rr          *roadReader
	rng         *rand.Rand
	selectivity int
	op          operationType

	center s2.LatLng
}

func makeQueryReader(path string, selectivity int, queryOp operationType) (*queryReader, error) {
	rr, err := makeRoadReader(path)
	if err != nil {
		return nil, err
	}
	qr := &queryReader{
		rr:          rr,
		rng:         rand.New(rand.NewSource(0)),
		selectivity: selectivity,
		op:          queryOp,
	}
	return qr, nil
}

func (qr *queryReader) Next() (query, bool) {
	for {
		road, ok := qr.rr.Next()
		if !ok {
			return query{}, false
		}
		if !(qr.rng.Intn(100) < qr.selectivity) {
			continue
		}
		q := query{
			center: road.lls[0],
			shape:  queryShape,
			op:     qr.op,
		}
		return q, ok
	}
}

type query struct {
	center s2.LatLng

	shape shapeType
	op    operationType
}

// ancestorCells returns the set of cells containing these cells, not including
// the given cells themselves.
func ancestorCells(cells ...s2.CellID) []s2.CellID {
	var ancestors []s2.CellID

	seen := make(map[s2.CellID]struct{})
	for _, c := range cells {
		for l := c.Level() - 1; l >= 0; l-- {
			a := c.Parent(l)
			if _, ok := seen[a]; ok {
				break
			}
			ancestors = append(ancestors, a)
			seen[a] = struct{}{}
		}
	}
	return ancestors
}

func containsQ(w *bytes.Buffer, cells []s2.CellID) {
	for i, c := range cells {
		if i != 0 {
			w.WriteString(` OR `)
		}
		fmt.Fprintf(w, `s2 BETWEEN '%s' AND '%s'`, c.RangeMin().ToToken(), c.RangeMax().ToToken())
	}
}

func containingQ(w *bytes.Buffer, cells []s2.CellID) {
	w.WriteString(`s2 IN (`)
	for i, c := range cells {
		if i != 0 {
			w.WriteString(`, `)
		}
		fmt.Fprintf(w, `'%s'`, c.ToToken())
	}
	w.WriteString(`)`)
}

var errQuerySkipped = errors.New(`skipped`)
var queryCoveringCounts [20]int64

func (q query) GetCovering(level int) []s2.CellID {
	var r s2.Region
	switch q.shape {
	case cellShape:
		r = s2.CellFromCellID(s2.CellIDFromLatLng(q.center).Parent(level))
	case capShape:
		arc := s1.Angle(arcFromLevel(level))
		r = s2.CapFromCenterAngle(s2.PointFromLatLng(q.center), arc)
	case rectShape:
		cell := s2.CellFromCellID(s2.CellIDFromLatLng(q.center).Parent(level))
		rect := s2.NewRectBounder()
		rect.AddPoint(cell.Vertex(0))
		rect.AddPoint(cell.Vertex(2))
		r = rect.RectBound()
	default:
		panic(`unhandled shape: ` + q.shape)
	}
	covering := cfg.Covering(r)
	if len(covering) > len(queryCoveringCounts) {
		panic("")
	}
	queryCoveringCounts[len(covering)]++
	return covering
}

func (q query) ReadSST(r *sstable.Reader, level int) (int, int, error) {
	covering := q.GetCovering(level)
	if covering == nil {
		// Couldn't do this covering.
		return 0, 0, errQuerySkipped
	}
	if q.op != containsOperation {
		panic("unsupported operation")
	}
	iter := r.NewIter(nil, nil)
	idMap := make(map[uint64]struct{})
	var rowCount int
	for _, c := range covering {
		first := []byte(c.RangeMin().ToToken())
		last := []byte(c.RangeMax().ToToken())
		var k *pebble.InternalKey
		var v []byte
		for k, v = iter.SeekGE(first); k != nil && bytes.Compare(k.UserKey, last) <= 0; k, v = iter.Next() {
			id, n := binary.Uvarint(v)
			if n <= 0 {
				panic("unable to parse varint")
			}
			if _, ok := idMap[id]; !ok {
				idMap[id] = struct{}{}
			}
			// idMap[id] = struct{}{}
			rowCount++
		}
	}

	if err := iter.Close(); err != nil {
		return 0, 0, err
	}
	return len(idMap), rowCount, nil
}

func (q query) ReadS2(db *sql.DB, cfg *s2IndexConfig, level int, printQuery bool) (int64, error) {
	covering := q.GetCovering(level)

	if covering == nil {
		// Couldn't do this covering.
		return 0, errQuerySkipped
	}

	var queryBuf bytes.Buffer
	queryBuf.WriteString(`SELECT `)
	if cfg.maxCells == 1 {
		// Take advantage of the fact that there's only one cell indexed for each
		// shape.
		queryBuf.WriteString(`count(id)`)
	} else {
		queryBuf.WriteString(`count(distinct(id))`)
	}
	queryBuf.WriteString(` FROM roads_s2_idx WHERE `)
	switch q.op {
	case containsOperation:
		containsQ(&queryBuf, covering)
	case containingOperation:
		containingQ(&queryBuf, append(ancestorCells(covering...), covering...))
		// containingQ(&queryBuf, append(ancestorCells(covering...)))
		// containingQ(&queryBuf, covering)
	case intersectsOperation:
		containsQ(&queryBuf, covering)
		queryBuf.WriteString(` OR `)
		// Don't append the covering cells themselves like we do for
		// containingOperation because the LIKE already handles it.
		containingQ(&queryBuf, ancestorCells(covering...))
	default:
		panic(`unhandled operation: ` + q.op)
	}

	// TODO(dan): Prepare all these. A little tricky since containing and
	// intersects each need a one version for each cell level.
	var count int64
	if printQuery && false {
		fmt.Printf("%s\n", queryBuf.String())
	}
	err := db.QueryRow(queryBuf.String()).Scan(&count)
	err = errors.Wrapf(err, `executing: %s`, queryBuf.String())
	return count, err
}

func (q query) ReadPostGIS(db *sql.DB, level int) (int64, error) {
	var geom string
	switch q.shape {
	case cellShape, rectShape:
		cell := s2.CellFromCellID(s2.CellIDFromLatLng(q.center).Parent(level))
		v0, v2 := s2.LatLngFromPoint(cell.Vertex(0)), s2.LatLngFromPoint(cell.Vertex(2))
		// Yeah, lng and lat and swapped from what you'd expect.
		geom = fmt.Sprintf(`ST_MakeEnvelope(%v, %v, %v, %v)`,
			v0.Lng.Degrees(), v0.Lat.Degrees(), v2.Lng.Degrees(), v2.Lat.Degrees())
	case capShape:
		// Yeah, lng and lat and swapped from what you'd expect.
		geom = fmt.Sprintf(`ST_Buffer(ST_MakePoint(%v, %v)::geography, %d)::geometry`,
			q.center.Lng.Degrees(), q.center.Lat.Degrees(), int(metersFromLevel(level)))
	default:
		panic(`unhandled shape: ` + q.shape)
	}

	var op string
	switch q.op {
	case containsOperation:
		op = `@`
	case containingOperation:
		op = `~`
	case intersectsOperation:
		op = `&&`
	default:
		panic(`unhandled operation: ` + q.op)
	}

	query := fmt.Sprintf(
		`WITH t (geom) AS (SELECT %s) SELECT count(id) FROM roads, t WHERE roads.geometry %s t.geom`,
		geom, op,
	)
	var count int64
	err := db.QueryRow(query).Scan(&count)
	err = errors.Wrapf(err, `executing: %s`, query)
	return count, err
}
