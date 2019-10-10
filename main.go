package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/codahale/hdrhistogram"
	"github.com/golang/geo/s2"
	_ "github.com/lib/pq"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

const (
	histMinLatency = 1 * time.Microsecond
	histMaxLatency = 100 * time.Second

	convertRows = 1000000

	queryMinLevel    = 8
	queryMaxLevel    = 20 // ~ 1 meter
	queryMaxCount    = (queryMaxLevel - queryMinLevel + 1) * 100
	querySelectivity = 1
	queryShape       = cellShape

	updateInterval    = time.Second
	earthRadiusMeters = 6378137 // lol which radius, it's not a sphere
)

var cfg = &crdbIndexConfig{
	minLevel: 0,
	maxLevel: 30,
	maxCells: 4,
}

type crdbIndexConfig struct {
	minLevel, maxLevel, maxCells int
	rc                           *s2.RegionCoverer
}

func (c *crdbIndexConfig) Covering(r s2.Region) []s2.CellID {
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

func convert(in, table, index string, cfg *crdbIndexConfig) error {
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
	var cellsBuf bytes.Buffer
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
		cellsBuf.Reset()
		for i, c := range covering {
			fmt.Fprintf(indexW, `%d,%s`+"\n", road.idx, c)
			if i != 0 {
				cellsBuf.WriteString(`,`)
			}
			cellsBuf.WriteString(c.ToToken())
		}
		fmt.Fprintf(tableW, `%d,%s,"%s","%s"`+"\n", road.idx, name, cellsBuf.String(), road.geometry)
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
	if _, err := db.Exec(`DROP TABLE IF EXISTS roads_s2_idx`); err != nil {
		return errors.Wrapf(err, "dropping existing data")
	}
	const importStmt = `IMPORT TABLE roads (id INT PRIMARY KEY, name STRING, covering STRING, geometry STRING) CSV DATA ($1)`
	if _, err := db.Exec(importStmt, table); err != nil {
		return errors.Wrapf(err, "importing: %s", table)
	}
	const importIdxStmt = `IMPORT TABLE roads_s2_idx (id INT, s2 STRING, PRIMARY KEY(s2, id)) CSV DATA ($1)`
	if _, err := db.Exec(importIdxStmt, index); err != nil {
		return errors.Wrapf(err, "importing: %s", index)
	}
	return nil
}

func pgLoad(conn, table string) error {
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
	if _, err := db.Exec(`COPY roads (id, name, geometry) FROM '` + table + `' CSV DELIMITER ','`); err != nil {
		return errors.Wrapf(err, "creating table")
	}
	if _, err := db.Exec(`CREATE INDEX ON roads USING GIST (geometry)`); err != nil {
		return errors.Wrapf(err, "creating table")
	}
	return nil
}

func run(conn, file string, cfg *crdbIndexConfig) error {
	db, err := sql.Open("postgres", conn)
	if err != nil {
		return errors.Wrapf(err, "connecting to: %s", conn)
	}

	allQueryOps := []operationType{containsOperation, containingOperation, intersectsOperation}
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

		if err := doRun(cfg, qr, db, nanosByLevel, countsByLevel); err != nil {
			return err
		}
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
	}

	return nil
}

func doRun(
	cfg *crdbIndexConfig,
	qr *queryReader,
	db *sql.DB,
	nanosByLevel, countsByLevel []*hdrhistogram.Histogram,
) error {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)
	g.Go(func() error {
		defer cancel()
		select {
		case <-ctx.Done():
		case <-signalChan:
			fmt.Printf("\nReceived an interrupt, stopping...\n")
			signal.Reset(os.Interrupt)
		}
		return nil
	})
	g.Go(func() error {
		defer cancel()
		start := time.Now()
		lastUpdate := start
		for i := 0; i < queryMaxCount; {
			if err := ctx.Err(); err != nil {
				return nil
			}
			q, ok := qr.Next()
			if !ok {
				break
			}
			if now := time.Now(); now.Sub(lastUpdate) > updateInterval {
				lastUpdate = now
				fmt.Printf("finished %d queries in %s\n", i, now.Sub(start))
			}

			qStart := time.Now()
			var level int
			var count int64
			var err error
			if cfg != nil {
				level, count, err = q.RunCRDB(db, cfg)
			} else {
				level, count, err = q.RunPostgres(db)
			}
			if err != nil {
				return err
			}

			// A level < 0 means non-error skip.
			if level < 0 {
				continue
			}
			i++
			nanosByLevel[level].RecordValue(time.Since(qStart).Nanoseconds())
			countsByLevel[level].RecordValue(count)
		}
		return nil
	})
	return g.Wait()
}

func main() {
	args := os.Args[1:]
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
		if len(subArgs) != 2 {
			log.Fatalf("usage: %s pgload <conn> <table.csv>", os.Args[0])
		}
		if err := pgLoad(subArgs[0], subArgs[1]); err != nil {
			log.Fatal(err)
		}
	case `crdbquery`, `pgquery`:
		subArgs := args[1:]
		if len(subArgs) != 2 {
			log.Fatalf("usage: %s %s <conn> <data.csv.bz2>", os.Args[0], args[0])
		}
		var crdbCfg *crdbIndexConfig
		if args[0] == `crdbquery` {
			crdbCfg = cfg
		}
		if err := run(subArgs[0], subArgs[1], crdbCfg); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("unknown command: %s", args[0])
	}
}
