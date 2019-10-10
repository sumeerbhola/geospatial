package main

import (
	"compress/bzip2"
	"encoding/csv"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/golang/geo/s2"
	"github.com/pkg/errors"
)

type roadReader struct {
	i       int
	f       *os.File
	r       *csv.Reader
	skipped int

	buf []s2.LatLng
}

type road struct {
	idx      int
	name     string
	geometry string
	lls      []s2.LatLng
}

func makeRoadReader(path string) (*roadReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.Wrapf(err, "opening %s", path)
	}
	r := csv.NewReader(bzip2.NewReader(f))
	r.FieldsPerRecord = 5
	r.Comma = '\t'
	r.ReuseRecord = true
	return &roadReader{f: f, r: r}, nil
}

func (rr *roadReader) Close() {
	rr.f.Close()
}

func (rr *roadReader) Next() (road, bool) {
	var linestringOrig, name string
	for {
		parts, err := rr.r.Read()
		if err == io.EOF {
			return road{}, false
		} else if err != nil {
			panic(err)
		}
		linestringOrig, name = parts[0], parts[2]
		if strings.HasPrefix(linestringOrig, `MULTILINESTRING`) {
			rr.skipped++
			continue
		}
		break
	}
	linestring := strings.TrimPrefix(linestringOrig, `LINESTRING (`)
	linestring = strings.TrimSuffix(linestring, `)`)

	rr.buf = rr.buf[:0]
	for len(linestring) > 0 {
		var llRaw string
		if i := strings.IndexByte(linestring, ','); i == -1 {
			llRaw, linestring = linestring, linestring[:0]
		} else {
			llRaw, linestring = linestring[:i], linestring[i+1:]
		}
		i := strings.IndexByte(llRaw, ' ')
		lng, err := strconv.ParseFloat(llRaw[:i], 64)
		if err != nil {
			panic(errors.Wrapf(err, "invalid linestring: %s", linestringOrig))
		}
		lat, err := strconv.ParseFloat(llRaw[i+1:], 64)
		if err != nil {
			panic(errors.Wrapf(err, "invalid linestring: %s", linestringOrig))
		}
		rr.buf = append(rr.buf, s2.LatLngFromDegrees(lat, lng))
	}
	r := road{
		idx:      rr.i,
		name:     name,
		geometry: linestringOrig,
		lls:      rr.buf,
	}
	rr.i++
	return r, true
}
