package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/csv"
	"flag"
	"fmt"
	"github.com/cockroachdb/pebble"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/pebble/sstable"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/codahale/hdrhistogram"
	"github.com/golang/geo/s2"
	_ "github.com/lib/pq"
	"github.com/pkg/errors"
)

var cpuprofile = flag.String("cpuprofile", "", "write cpu profile to file")

const (
	histMinLatency = 1 * time.Microsecond
	histMaxLatency = 100 * time.Second

	convertRows = 1000000

	queryMinLevel    = 8
	queryMaxLevel = 8
	// queryMaxLevel    = 20 // ~ 1 meter
	querySelectivity = 1
	queryShape       = rectShape

	latenciesMaxCount     = (queryMaxLevel - queryMinLevel + 1) * 100
	throughputMaxDuration = time.Second

	updateInterval    = time.Second
	earthRadiusMeters = 6371010 // From the c++ s2 library.
)

var cfg = &s2IndexConfig{
	minLevel: 0,
	maxLevel: 30,
	maxCells: 16,
}

type s2IndexConfig struct {
	minLevel, maxLevel, maxCells int
	rc                           *s2.RegionCoverer
}

func (c *s2IndexConfig) Covering(r s2.Region) []s2.CellID {
	if c, ok := r.(s2.Cell); ok {
		return []s2.CellID{c.ID()}
	}
	if c.rc == nil {
		c.rc = &s2.RegionCoverer{MinLevel: c.minLevel, MaxLevel: c.maxLevel, MaxCells: c.maxCells}
	}
	covering := c.rc.Covering(r)
	if c.maxCells == 1 && len(covering) != 1 {
		// We covered a cube edge or corner and couldn't do 1 cell.
		return nil
	}
	return covering
}

func convert(in, table, index string, cfg *s2IndexConfig) error {
	rr, err := makeRoadReader(in)
	if err != nil {
		return err
	}
	defer rr.Close()

	tableW, err := os.Create(table)
	if err != nil {
		return err
	}
	defer tableW.Close()
	indexW, err := os.Create(index)
	if err != nil {
		return err
	}
	defer indexW.Close()

	start := time.Now()
	lastUpdate := start
	for i := 0; i < convertRows; {
		road, ok := rr.Next()
		if !ok {
			break
		}
		if now := time.Now(); now.Sub(lastUpdate) > updateInterval {
			lastUpdate = now
			fmt.Printf("finished %d queries in %s\n", i, now.Sub(start))
		}

		name := strings.ReplaceAll(road.name, `,`, ``)
		polyline := s2.PolylineFromLatLngs(road.lls)
		covering := cfg.Covering(polyline)
		if covering == nil {
			continue
		}
		i++
		for _, c := range covering {
			fmt.Fprintf(indexW, `%d,%s`+"\n", road.idx, c.ToToken())
		}
		fmt.Fprintf(tableW, `%d,"%s","%s"`+"\n", road.idx, name, road.geometry)
	}
	if err := tableW.Sync(); err != nil {
		return err
	}
	if err := indexW.Sync(); err != nil {
		return err
	}
	if rr.skipped > 0 {
		fmt.Printf("skipped %d\n", rr.skipped)
	}
	return nil
}

func crdbLoad(conn, table, index string) error {
	db, err := sql.Open("postgres", conn)
	if err != nil {
		return errors.Wrapf(err, "connecting to: %s", conn)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS roads`); err != nil {
		return errors.Wrapf(err, "dropping existing data")
	}
	const importStmt = `IMPORT TABLE roads (id INT PRIMARY KEY, name STRING, geometry STRING) CSV DATA ($1)`
	if _, err := db.Exec(importStmt, table); err != nil {
		return errors.Wrapf(err, "importing: %s", table)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS roads_s2_idx`); err != nil {
		return errors.Wrapf(err, "dropping existing data")
	}
	start := time.Now()
	const importIdxStmt = `IMPORT TABLE roads_s2_idx (id INT, s2 STRING, PRIMARY KEY(s2, id)) CSV DATA ($1)`
	if _, err := db.Exec(importIdxStmt, index); err != nil {
		return errors.Wrapf(err, "importing: %s", index)
	}
	fmt.Printf("loaded in %s\n", time.Since(start))
	return nil
}

type s2Id struct {
	cellid string
	id string
}

type sortIndex []s2Id
func (s sortIndex) Len() int { return len(s) }
func (s sortIndex) Less(i, j int) bool {
	cmp := strings.Compare(s[i].cellid, s[j].cellid)
	if cmp != 0 {
		return cmp < 0
	}
	return strings.Compare(s[i].id, s[j].id) < 0
}
func (s sortIndex) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func sstLoad(indexInput string, indexOutput string) error {
	f, err := os.Open(indexInput)
	if err != nil {
		return errors.Wrapf(err, "opening %s", indexInput)
	}
	r := csv.NewReader(f)
	r.FieldsPerRecord = 2
	r.ReuseRecord = true

	index := make([]s2Id, 0, 4000000)
	for {
		parts, err := r.Read()
		if err == io.EOF {
			break
		} else if err != nil {
			panic(err)
		}
		index = append(index, s2Id{cellid: parts[1], id: parts[0]})
	}
	f.Close()
	sort.Sort(sortIndex(index))
	fmt.Printf("read %d records\n", len(index))
	of, err := vfs.Default.Create(indexOutput)
	if err != nil {
		return errors.Wrapf(err, "creating %s", indexOutput)
	}
	opts := sstable.WriterOptions{
		TableFormat: sstable.TableFormatRocksDBv2,
		MergerName: "nullptr",
	}
	w := sstable.NewWriter(of, opts)
	for i := range index {
		if i % 1000 == 0 {
			fmt.Printf("%s, %s\n", index[i].cellid, index[i].id)
		}
		id, err := strconv.Atoi(index[i].id)
		if err != nil {
			panic(err)
		}
		buf := make([]byte, binary.MaxVarintLen64)
		n := binary.PutUvarint(buf, uint64(id))
		w.Set([]byte(index[i].cellid), buf[:n])
	}
	if err := w.Close(); err != nil {
		return errors.Wrapf(err, "closing sst writer")
	}
	return nil
}

func pgLoad(conn, table, index string) error {
	db, err := sql.Open("postgres", conn)
	if err != nil {
		return errors.Wrapf(err, "connecting to: %s", conn)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS roads`); err != nil {
		return errors.Wrapf(err, "dropping existing data")
	}
	if _, err := db.Exec(`CREATE TABLE roads (id INT PRIMARY KEY, name VARCHAR, geometry GEOMETRY)`); err != nil {
		return errors.Wrapf(err, "creating table")
	}
	start := time.Now()
	if _, err := db.Exec(`COPY roads (id, name, geometry) FROM '` + table + `' CSV DELIMITER ','`); err != nil {
		return errors.Wrapf(err, "creating table")
	}
	fmt.Printf("loaded in %s\n", time.Since(start))
	start = time.Now()
	if _, err := db.Exec(`CREATE INDEX ON roads USING GIST (geometry)`); err != nil {
		return errors.Wrapf(err, "creating table")
	}
	fmt.Printf("created index in %s\n", time.Since(start))
	if _, err := db.Exec(`DROP TABLE IF EXISTS roads_s2_idx`); err != nil {
		return errors.Wrapf(err, "dropping existing data")
	}
	if _, err := db.Exec(`CREATE TABLE roads_s2_idx (id INT, s2 VARCHAR, PRIMARY KEY(s2, id))`); err != nil {
		return errors.Wrapf(err, "creating table")
	}
	if _, err := db.Exec(`COPY roads_s2_idx (id, s2) FROM '` + index + `' CSV DELIMITER ','`); err != nil {
		return errors.Wrapf(err, "creating table")
	}
	return nil
}

func ctrlcCtx() (ctx context.Context, cancel func()) {
	ctx, cancel = context.WithCancel(context.Background())
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)
	go func() {
		select {
		case <-signalChan:
			fmt.Println("\nReceived an interrupt, stopping...")
			signal.Reset(os.Interrupt)
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func latencies(conn, file string, cfg *s2IndexConfig) error {
	ctx, cancel := ctrlcCtx()
	defer cancel()

	db, err := sql.Open("postgres", conn)
	if err != nil {
		return errors.Wrapf(err, "connecting to: %s", conn)
	}

	// allQueryOps := []operationType{containsOperation, containingOperation, intersectsOperation}
	allQueryOps := []operationType{containsOperation}

	firstTime := true
	ForLoop:
	for i, queryOp := range allQueryOps {
		if i != 0 {
			fmt.Println()
		}
		fmt.Printf("starting query type %s shape %s\n", queryOp, queryShape)
		const histSigFigs = 1
		nanosByLevel := make([]*hdrhistogram.Histogram, queryMaxLevel+1)
		for i := range nanosByLevel {
			nanosByLevel[i] = hdrhistogram.New(
				histMinLatency.Nanoseconds(), histMaxLatency.Nanoseconds(), histSigFigs)
		}
		countsByLevel := make([]*hdrhistogram.Histogram, queryMaxLevel+1)
		for i := range countsByLevel {
			countsByLevel[i] = hdrhistogram.New(0, 10000000, histSigFigs)
		}

		qr, err := makeQueryReader(file, querySelectivity, queryOp)
		if err != nil {
			return err
		}

		start := time.Now()
		lastUpdate := start
		level := -1
		for i := range queryCoveringCounts {
			queryCoveringCounts[i] = 0
		}
		for i := 0; i < latenciesMaxCount; level-- {
			if err := ctx.Err(); err != nil {
				break
			}
			q, ok := qr.Next()
			if !ok {
				break
			}
			if now := time.Now(); now.Sub(lastUpdate) > updateInterval {
				lastUpdate = now
				fmt.Printf("finished %d queries in %s\n", i, now.Sub(start))
			}

			if level < queryMinLevel {
				level = queryMaxLevel
			}
			qStart := time.Now()
			var count int64
			var err error
			if cfg != nil {
				count, err = q.ReadS2(db, cfg, level, firstTime)
			} else {
				count, err = q.ReadPostGIS(db, level)
			}
			qDuration := time.Since(qStart)
			if err == errQuerySkipped {
				continue
			} else if err != nil {
				return err
			}
			i++
			nanosByLevel[level].RecordValue(qDuration.Nanoseconds())
			countsByLevel[level].RecordValue(count)
		}
		firstTime = false
		fmt.Printf("finished query type %s shape %s\n", queryOp, queryShape)
		fmt.Println("level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax")
		for level := range nanosByLevel {
			nanosHist, countsHist := nanosByLevel[level], countsByLevel[level]
			if nanosHist.TotalCount() == 0 {
				continue
			}
			fmt.Printf("%5d %7d %8d %8.2f %8.2f %8.2f %8.2f %8.2f %8d %8d %8d %8d\n",
				level,
				int(metersFromLevel(level)),
				nanosHist.TotalCount(),
				time.Duration(nanosHist.Min()).Seconds()*1000,
				time.Duration(nanosHist.ValueAtQuantile(50)).Seconds()*1000,
				time.Duration(nanosHist.ValueAtQuantile(95)).Seconds()*1000,
				time.Duration(nanosHist.ValueAtQuantile(99)).Seconds()*1000,
				time.Duration(nanosHist.Max()).Seconds()*1000,
				countsHist.ValueAtQuantile(50),
				countsHist.ValueAtQuantile(95),
				countsHist.ValueAtQuantile(99),
				countsHist.Max(),
			)
		}
		fmt.Printf("query covering counts: ")
		for i := range queryCoveringCounts {
			fmt.Printf("%d: %d, ", i, queryCoveringCounts[i])
		}
		fmt.Println()
		if err := ctx.Err(); err != nil {
			return nil
		}
	}
	goto ForLoop

	return nil
}

func sstlatencies(sstFile, queryFile string) error {
	ctx, cancel := ctrlcCtx()
	defer cancel()

	f, err := vfs.Default.Open(sstFile)
	if err != nil {
		return err
	}
	opts := sstable.ReaderOptions{Cache: pebble.NewCache(128 << 20)}
	defer opts.Cache.Unref()
	r, err := sstable.NewReader(f, opts)
	if err != nil {
		return err
	}
	defer r.Close()
	// allQueryOps := []operationType{containsOperation, containingOperation, intersectsOperation}
	allQueryOps := []operationType{containsOperation}

	numLoops := 0
ForLoop:
	for i, queryOp := range allQueryOps {
		if i != 0 {
			fmt.Println()
		}
		fmt.Printf("starting query type %s shape %s\n", queryOp, queryShape)
		const histSigFigs = 1
		nanosByLevel := make([]*hdrhistogram.Histogram, queryMaxLevel+1)
		for i := range nanosByLevel {
			nanosByLevel[i] = hdrhistogram.New(
				histMinLatency.Nanoseconds(), histMaxLatency.Nanoseconds(), histSigFigs)
		}
		countsByLevel := make([]*hdrhistogram.Histogram, queryMaxLevel+1)
		for i := range countsByLevel {
			countsByLevel[i] = hdrhistogram.New(0, 10000000, histSigFigs)
		}
		rowsByLevel := make([]*hdrhistogram.Histogram, queryMaxLevel+1)
		for i := range rowsByLevel {
			rowsByLevel[i] = hdrhistogram.New(0, 10000000, histSigFigs)
		}

		qr, err := makeQueryReader(queryFile, querySelectivity, queryOp)
		if err != nil {
			return err
		}

		start := time.Now()
		lastUpdate := start
		level := -1
		for i := range queryCoveringCounts {
			queryCoveringCounts[i] = 0
		}
		for i := 0; i < latenciesMaxCount; level-- {
			if err := ctx.Err(); err != nil {
				break
			}
			q, ok := qr.Next()
			if !ok {
				break
			}
			if now := time.Now(); now.Sub(lastUpdate) > updateInterval {
				lastUpdate = now
				fmt.Printf("finished %d queries in %s\n", i, now.Sub(start))
			}

			if level < queryMinLevel {
				level = queryMaxLevel
			}
			qStart := time.Now()
			count, rowCount, err := q.ReadSST(r, level)
			qDuration := time.Since(qStart)
			if err == errQuerySkipped {
				continue
			} else if err != nil {
				return err
			}
			i++
			nanosByLevel[level].RecordValue(qDuration.Nanoseconds())
			countsByLevel[level].RecordValue(int64(count))
			rowsByLevel[level].RecordValue(int64(rowCount))
		}
		fmt.Printf("finished query type %s shape %s\n", queryOp, queryShape)
		fmt.Println("level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax__r50__r95__r99_rMax")
		for level := range nanosByLevel {
			nanosHist, countsHist, rowsHist := nanosByLevel[level], countsByLevel[level], rowsByLevel[level]
			if nanosHist.TotalCount() == 0 {
				continue
			}
			fmt.Printf("%5d %7d %8d %8.2f %8.2f %8.2f %8.2f %8.2f %8d %8d %8d %8d %8d %8d %8d %8d\n",
				level,
				int(metersFromLevel(level)),
				nanosHist.TotalCount(),
				time.Duration(nanosHist.Min()).Seconds()*1000,
				time.Duration(nanosHist.ValueAtQuantile(50)).Seconds()*1000,
				time.Duration(nanosHist.ValueAtQuantile(95)).Seconds()*1000,
				time.Duration(nanosHist.ValueAtQuantile(99)).Seconds()*1000,
				time.Duration(nanosHist.Max()).Seconds()*1000,
				countsHist.ValueAtQuantile(50),
				countsHist.ValueAtQuantile(95),
				countsHist.ValueAtQuantile(99),
				countsHist.Max(),
				rowsHist.ValueAtQuantile(50),
				rowsHist.ValueAtQuantile(95),
				rowsHist.ValueAtQuantile(99),
				rowsHist.Max(),
			)
		}
		fmt.Printf("query covering counts: ")
		for i := range queryCoveringCounts {
			fmt.Printf("%d: %d, ", i, queryCoveringCounts[i])
		}
		fmt.Println()
		if err := ctx.Err(); err != nil {
			return nil
		}
	}
	numLoops++
	if numLoops < 20 {
		goto ForLoop
	}

	return nil
}

func NewSSTReader(path string) (*sstable.Reader, error) {
	file, err := vfs.Default.Open(path)
	if err != nil {
		return nil, err
	}
	return sstable.NewReader(file, sstable.ReaderOptions{})
}

func main() {
	var args []string
	flag.Parse()
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
		args = os.Args[2:]
	} else {
		args = os.Args[1:]
	}
	if len(args) < 1 {
		log.Fatalf("usage: %s <cmd>", os.Args[0])
	}
	switch args[0] {
	case `convert`:
		subArgs := args[1:]
		if len(subArgs) != 3 {
			log.Fatalf("usage: %s convert <in.csv.bz2> <table.csv> <index.csv>", os.Args[0])
		}
		if err := convert(subArgs[0], subArgs[1], subArgs[2], cfg); err != nil {
			log.Fatal(err)
		}
	case `crdbload`:
		subArgs := args[1:]
		if len(subArgs) != 3 {
			log.Fatalf("usage: %s crdbload <conn> <table.csv> <index.csv>", os.Args[0])
		}
		if err := crdbLoad(subArgs[0], subArgs[1], subArgs[2]); err != nil {
			log.Fatal(err)
		}
	case `pgload`:
		subArgs := args[1:]
		if len(subArgs) != 3 {
			log.Fatalf("usage: %s pgload <conn> <table.csv> <index.csv>", os.Args[0])
		}
		if err := pgLoad(subArgs[0], subArgs[1], subArgs[2]); err != nil {
			log.Fatal(err)
		}
	case `sstload`:
		subArgs := args[1:]
		if len(subArgs) != 2 {
			log.Fatalf("usage: %s sstload <index.csv> <index.sst>", os.Args[0])
		}
		if err := sstLoad(subArgs[0], subArgs[1]); err != nil {
			log.Fatal(err)
		}
	case `s2latencies`, `pglatencies`:
		subArgs := args[1:]
		if len(subArgs) != 2 {
			log.Fatalf("usage: %s %s <conn> <data.csv.bz2>", os.Args[0], args[0])
		}
		var s2Cfg *s2IndexConfig
		if args[0] == `s2latencies` {
			s2Cfg = cfg
		}
		if err := latencies(subArgs[0], subArgs[1], s2Cfg); err != nil {
			log.Fatal(err)
		}
	case `sstlatencies`:
		subArgs := args[1:]
		if len(subArgs) != 2 {
			log.Fatalf("usage: %s %s <sst> <data.csv.bz2>", os.Args[0], args[0])
		}
		if err := sstlatencies(subArgs[0], subArgs[1]); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("unknown command: %s", args[0])
	}
}
