geospatial
==========

This repository attempts to compare the performance of an [S2] based geospatial
indexing strategy in CockroachDB to the PostGIS implementation in Postgres.


# Spatial Indexes

A spatial index is commonly used on top of objects existing in an infinite 2d
plane (projected maps) or a sphere (the Earth). This index can be used to speed
up queries of various types: contains (what indexed objects does this object
contain), containing (what indexed objects contain this object), intersection
(what indexes objects overlap this object), and nearest-neighbors (what are the
K nearest objects in the index to this object).

Typically, geospatial indexes operate using approximations of the objects they
contain. Example: PostGIS indexes consist of rectangular bounding boxes. This
means the index will not give exact answers, it will give a list with false
positives but no false negatives. A slower exact computation will then be run on
the results from the index. (Note that from an index's perspective,
"intersection" is simply the union of "contains" and "containing").

Implemented here is a possible mechanism for building geospatial indexes in
CockroachDB using the S2 geometry library. Unlike the divide-the-objects
approach used in PostGIS, S2's divide-the-space approach maps much better to
CockroachDB's KV model. As explained in the [SIFT] paper, divide-the-space also
maps much better to horizontal distribution.

A full exploration of how [S2] works is out of scope of this README (the link
has a lot of background), but at a high level, a sphere representing the Earth
is divided into 6 30-level quadtrees of "cells". These cells are
content-addressed, meaning it's possible to map the identifier of the cell to
its position in the tree with no external information. To index an object, a
"covering" of one or more cells is computed and saved in an inverted index of
cell -> primary key. Queries of types "contains" and type "containing" then can
be implemented using two key properties of S2 cell identifiers.

- There is a function that maps S2 cell identifers to strings (ditto for
  uint64s) such that every cell (of every size) contained by a cell is
  expressible as a range query (which returns no unrelated cells). This is how
  "contains" is implemented. Compute a covering of the query object and, for
  each cell in the covering, run a range query on the inverted index, union'ing
  and distinct'ing the results.

- This same mapping also allows computing the string (or uint64) for each
  "parent" (containing) of a given cell. A "containing" query is then the
  union+distinct of all parent cells of the cells in a covering. In the worst
  case, this is `O(cells in covering) * O(levels in tree = 31)`, but typical
  coverings will share most of their parent cells, making this much closer to
  `O(cells in covering) + O(levels in tree = 31)` in practice.

[s2]: http://s2geometry.io/
[SIFT]: https://www.anand-iyer.com/papers/sift-socc2017.pdf


# Performance Results

A real world dataset of roads was converted into CSV and imported into both
CockroachDB and PostGIS. A second table representing the inverted index that
CockroachDB would use was created and imported only into CockroachDB.

The index part of "contains", "containing", and "intersects" operations were
benchmarked, skipping the followup exact match filter for now.
"nearest-neighbor" is not implemented yet. The benchmarks come in three flavors
of needle shape: cell (the exact bounds of an S2 cell - best case for S2), cap
(everything within some distance of a point), and rect (a rectangular box). The
query shapes were scaled from ~1 meter to ~6000 meters to see the effect of
fetching more data.

In the following, `meters` is the approximate scaling, `numQ` is the number of
queries actually run, `p*` are latency percentiles, and `count*` are percentiles
for how many results were returned.

## Shape: cell

- Both index time and query time were run with 4-cell coverings here. Defaults
  for these are something to tune (and possibly will end up as an index option
  for advanced users).
- This corresponds to exactly one S2 cell, the best case for S2.
- Note that a very similar number of results are returned for both impls. This
  is good as they'd have to run the exact filtering computation on a similar
  number of things.
- For the most part, CockroachDB appears to be about half as fast. Unclear
  whether this is due to our geospatial indexing or due to more general
  performances differences between the two systems.

  Note that CockroachDB has a baseline latency that is almost double that of
  postgres: 0.48ms vs 0.22 ms. However, each query was returning a count, so I
  assume this overhead disappears in the noise for larger query sizes.
- **Key insight** One big difference between PostGIS and our S2 impl is that
  each primary key is stored once per cell in the covering in CockroachDB but
  exactly once in Postgres. This means the Cockroach retrieval query had to
  `distinct` before it counts but PostGIS doesn't. To attempt to measure this, I
  ran with 1 cell coverings in CockroachDB, which then means we can skip the
  distinct. This generates some [absurd coverings], so it probably isn't viable,
  but it should give us an idea of the overhead.

[absurd coverings]: img/absurd-1cell-covering.png


#### Operation: contains

S2 in CockroachDB (4 cell index/4 cell query)

```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8   39092      100     1.11     9.44    18.87    46.14    48.23     3711     6399    17407    17407
    9   19546      100     0.92     3.15     6.55     8.91    10.49      863     2047     3199     3967
   10    9773      100     0.72     1.31     3.41     5.24     6.03      255      895     1343     1727
   11    4886      100     0.62     0.95     2.03     3.54     3.80       75      367      495      639
   12    2443      100     0.51     0.75     1.18     2.62     3.80       22      123      159      247
   13    1221      100     0.49     0.72     1.11     1.18     1.25       12       57       71       75
   14     610      100     0.51     0.72     1.05     1.70     3.41        4       22       37       51
   15     305      100     0.49     0.69     1.64     4.72     5.51        3        9       20       20
   16     152      100     0.52     0.69     1.18     3.15     4.06        2        6        8        8
   17      76      100     0.52     0.69     1.11     1.44     3.28        1        4        4        5
   18      38      100     0.52     0.69     1.05     1.18     1.90        1        3        6        6
   19      19      100     0.51     0.69     1.18     1.57     3.41        0        2        3        3
   20       9      100     0.59     0.92     1.11     3.01    12.06        0        1        2        2
```

PostGIS
```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8   39092      100     0.59     2.49    14.68    20.97    31.46     3583     6399    17407    17407
    9   19546      100     0.52     1.11     7.08     8.13    13.11      799     1983     3071     3967
   10    9773      100     0.36     0.62     3.28     4.19     4.46      223      831     1279     1599
   11    4886      100     0.33     0.49     3.01     3.28     4.98       57      303      463      575
   12    2443      100     0.28     0.41     2.10     3.93     4.46       14       99      107      215
   13    1221      100     0.23     0.39     1.64     3.15     3.28        6       37       41       43
   14     610      100     0.22     0.39     1.18     3.01     3.67        2       12       17       22
   15     305      100     0.25     0.36     1.57     2.23     5.51        1        4        6       14
   16     152      100     0.26     0.36     0.82     1.57     1.77        0        2        4        5
   17      76      100     0.26     0.36     0.85     2.88    19.92        0        1        2        3
   18      38      100     0.28     0.36     0.56     3.01     3.41        0        1        2        4
   19      19      100     0.25     0.36     0.72     3.15     3.28        0        0        0        3
   20       9      100     0.28     0.39     0.62     1.97    26.21        0        0        0        0
```

S2 in CockroachDB (DISTINCT unnecessary/1 cell index/1 cell query)

```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8   39092      100     0.66     2.03     3.67     7.34     7.86     3455     6143    17407    17407
    9   19546      100     0.59     1.05     1.64     2.10     2.23      767     1919     3071     3839
   10    9773      100     0.56     0.79     1.18     2.23     4.98      223      799     1279     1535
   11    4886      100     0.56     0.72     0.98     1.18     1.25       55      303      447      575
   12    2443      100     0.56     0.69     0.98     1.11     1.18       14       99      107      215
   13    1221      100     0.52     0.69     1.02     1.05     1.64        6       37       41       41
   14     610      100     0.46     0.72     0.88     1.18     2.62        2       12       17       21
   15     305      100     0.46     0.69     1.02     2.75     3.01        1        3        6       14
   16     152      100     0.48     0.69     0.95     1.18     1.18        0        2        4        5
   17      76      100     0.52     0.69     0.98     1.25     1.57        0        1        2        3
   18      38      100     0.51     0.69     0.92     1.11     1.25        0        1        2        4
   19      19      100     0.52     0.69     1.02     1.25     1.38        0        0        0        3
   20       9      100     0.52     0.72     0.98     1.05     5.77        0        0        0        0
```

S2 in CockroachDB (DISTINCT unnecessary/1 cell index/4 cell query)

```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8   39092      100     0.82     3.54     5.77    15.20    15.73     3455     6143    17407    17407
    9   19546      100     0.72     1.44     2.49     3.15     4.06      767     1919     3071     3839
   10    9773      100     0.59     0.92     1.57     1.97     2.03      223      799     1279     1535
   11    4886      100     0.59     0.82     1.18     1.31     1.38       55      303      447      575
   12    2443      100     0.56     0.72     1.18     1.25     1.44       14       99      107      215
   13    1221      100     0.52     0.72     1.02     1.25     1.25        6       37       41       41
   14     610      100     0.52     0.72     1.11     1.25     3.28        2       12       17       21
   15     305      100     0.52     0.72     1.02     1.25     1.51        1        3        6       14
   16     152      100     0.51     0.72     0.95     1.18     1.25        0        2        4        5
   17      76      100     0.52     0.69     0.98     1.25     1.38        0        1        2        3
   18      38      100     0.52     0.72     1.05     1.25     1.51        0        1        2        4
   19      19      100     0.56     0.75     1.05     1.25     1.25        0        0        0        3
   20       9      100     0.59     0.82     1.11     3.15    11.01        0        0        0        0
```

S2 in CockroachDB (counts are wrong/4 cell index/1 cell query)

```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8   39092      100     1.05     6.03    12.06    27.26    27.26    14335    25599    69631    69631
    9   19546      100     0.72     1.97     4.46     6.29    39.85     3199     7935    12799    15359
   10    9773      100     0.56     0.98     2.03     2.62     3.01      959     3327     5375     6399
   11    4886      100     0.49     0.75     1.25     1.38     1.90      271     1343     1919     2431
   12    2443      100     0.44     0.66     0.98     1.05     1.25       71      447      511      927
   13    1221      100     0.43     0.62     0.88     0.95     1.25       33      183      207      223
   14     610      100     0.43     0.62     0.85     1.11     1.11       11       63       91      143
   15     305      100     0.44     0.59     0.88     1.02     1.11        7       25       43       61
   16     152      100     0.44     0.59     0.82     1.18     1.57        4       13       22       24
   17      76      100     0.43     0.59     0.82     0.98     1.11        1        8       12       12
   18      38      100     0.39     0.59     0.75     0.92     0.92        1        7       14       20
   19      19      100     0.38     0.62     0.98     1.18     1.77        0        3        5        9
   20       9      100     0.48     0.72     0.95     3.80     4.72        0        2        2        3
```

S2 in CockroachDB without DISTINCT (counts are wrong/4 cell index/4 cell query)

```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8   39092      100     1.02     7.86    14.68    37.75    37.75    14335    25599    69631    69631
    9   19546      100     0.92     2.49     5.24     6.55     8.91     3199     7935    12799    15359
   10    9773      100     0.59     1.11     2.62     3.41     3.93      959     3327     5375     6399
   11    4886      100     0.52     0.85     1.51     1.64     1.84      271     1343     1919     2431
   12    2443      100     0.51     0.72     1.11     1.25     1.31       71      447      511      927
   13    1221      100     0.49     0.69     1.02     1.18     1.38       33      183      207      223
   14     610      100     0.48     0.66     0.95     1.11     1.18       11       63       91      143
   15     305      100     0.46     0.62     0.95     1.11     1.25        7       25       43       61
   16     152      100     0.48     0.62     0.88     0.98     1.02        4       13       22       24
   17      76      100     0.48     0.62     0.95     1.05     1.11        1        8       12       12
   18      38      100     0.48     0.62     0.92     1.18     1.18        1        7       14       20
   19      19      100     0.49     0.66     1.02     1.05     1.51        0        3        5        9
   20       9      100     0.56     0.82     1.05     1.44    11.01        0        2        2        3
```

S2 in Postgres (4 cell index/4 cell query)

```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8   39092      100     2.49    33.55    88.08   130.02   184.55     3711     6399    17407    17407
    9   19546      100     2.10     8.91    24.12    29.36    30.41      863     2047     3199     3967
   10    9773      100     0.75     2.75     7.34    11.53    13.11      255      895     1343     1727
   11    4886      100     0.39     1.18     4.98     5.77     6.03       75      367      495      639
   12    2443      100     0.29     0.62     2.23     4.19     4.72       22      123      159      247
   13    1221      100     0.24     0.56     2.49     2.88     3.41       12       57       71       75
   14     610      100     0.21     0.39     0.88     2.62     2.75        4       22       37       51
   15     305      100     0.25     0.34     1.05     2.88    16.25        3        9       20       20
   16     152      100     0.21     0.33     0.72     0.98     2.62        2        6        8        8
   17      76      100     0.22     0.33     0.88     2.36     3.01        1        4        4        5
   18      38      100     0.23     0.33     0.72     2.49     2.49        1        3        6        6
   19      19      100     0.25     0.34     2.36     2.62     2.62        0        2        3        3
   20       9      100     0.28     0.41     0.79     2.49     9.44        0        1        2        2
```

#### Operation: containing

S2 in CockroachDB (4 cell index/4 cell query)

```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8   39092      100     0.52     0.82     1.18     1.31     1.31        1        4        8        8
    9   19546      100     0.52     0.85     1.18     1.64     1.90        3        6        8       11
   10    9773      100     0.56     0.85     1.25     1.38     1.38        4        8        9       11
   11    4886      100     0.69     0.88     1.31     1.44     1.51        5       10       13       19
   12    2443      100     0.69     0.88     1.38     1.44     1.97        5       13       15       15
   13    1221      100     0.69     0.95     1.44     1.77     3.15        7       12       16       19
   14     610      100     0.75     0.95     1.51     5.24     5.51        7       13       17       20
   15     305      100     0.75     0.98     1.44     1.90    31.46        8       14       20       24
   16     152      100     0.75     0.98     1.31     6.03     8.91        8       14       17       28
   17      76      100     0.75     0.98     1.38     1.51     1.90        9       14       21       33
   18      38      100     0.79     1.02     1.38     3.28     3.41        9       17       20       22
   19      19      100     0.85     1.05     1.64     3.41     3.67        8       16       21       23
   20       9      100     0.82     1.11     1.51     1.70     1.77        9       18       22       24
```

PostGIS
```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8   39092      100     0.23     0.31     0.51     0.56     0.59        0        0        0        0
    9   19546      100     0.25     0.31     0.51     0.66     0.82        0        4        4        6
   10    9773      100     0.25     0.31     0.39     0.51     0.52        0        4        5        5
   11    4886      100     0.25     0.33     0.48     0.56     0.56        0        4        5        6
   12    2443      100     0.25     0.34     0.59     0.59     1.25        1        5        6        6
   13    1221      100     0.26     0.34     0.49     0.56     0.62        2        6        7        9
   14     610      100     0.26     0.36     0.46     0.51     0.59        2        6        8        9
   15     305      100     0.28     0.36     0.48     0.59     0.59        3        7        9       12
   16     152      100     0.28     0.36     0.46     0.59     0.69        3        8       12       14
   17      76      100     0.26     0.36     0.51     0.59     0.62        3        7        9       10
   18      38      100     0.26     0.36     0.48     0.62     0.69        3        8        9       12
   19      19      100     0.28     0.38     0.56     0.59     0.59        3        9       13       13
   20       9      100     0.28     0.38     0.51     0.62     0.88        4        9       10       11
```


#### Operation: intersects

S2 in CockroachDB (4 cell index/4 cell query)

```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8   39092      100     2.23    17.83    32.51    83.89    83.89     3711     6399    17407    17407
    9   19546      100     1.70     5.24    10.49    16.25    19.92      863     2047     3327     3967
   10    9773      100     1.25     2.36     6.55     7.60     8.91      271      895     1343     1727
   11    4886      100     1.11     1.64     3.01     3.67     3.80       87      367      511      639
   12    2443      100     1.05     1.44     2.75     4.06     6.82       26      135      167      255
   13    1221      100     1.05     1.38     1.70     1.90     4.19       19       67       83       83
   14     610      100     1.02     1.38     1.77     2.10     3.67       11       30       45       55
   15     305      100     1.11     1.38     1.77     4.46     5.51       11       22       30       33
   16     152      100     1.11     1.38     1.90     2.23     4.46       10       17       22       28
   17      76      100     1.05     1.38     1.84     3.15     5.77        9       16       23       35
   18      38      100     1.11     1.38     1.77     2.23     2.23        9       18       21       23
   19      19      100     1.11     1.44     1.97     2.10     2.36        9       17       21       26
   20       9      100     1.25     1.70     2.36     4.46     8.13        9       18       22       24
```

PostGIS
```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8   39092      100     0.69     1.97     5.24     6.03     6.55     3711     6655    17407    17407
    9   19546      100     0.56     0.92     1.57     1.90     2.23      863     2175     3327     4095
   10    9773      100     0.41     0.66     0.98     1.25     1.51      271      895     1407     1727
   11    4886      100     0.38     0.56     0.75     0.82     0.82       87      367      511      639
   12    2443      100     0.38     0.49     0.69     0.79     0.85       26      135      167      255
   13    1221      100     0.36     0.48     0.62     0.75     0.75       17       67       75       87
   14     610      100     0.36     0.46     0.62     0.72     0.75        9       28       37       57
   15     305      100     0.36     0.46     0.62     0.72     0.75        9       18       26       30
   16     152      100     0.38     0.44     0.56     0.59     0.59        7       13       15       16
   17      76      100     0.34     0.44     0.56     0.69     0.72        7       13       14       16
   18      38      100     0.36     0.44     0.56     0.72     0.72        6       11       14       15
   19      19      100     0.34     0.46     0.66     0.69     0.72        6       13       17       21
   20       9      100     0.38     0.46     0.62     0.72     0.98        6       11       12       14
```


# Reproduction Steps

- Download the [sample dataset of roads] courtesy of [University of Minnesota].

- Run `go run *.go convert roads.csv.bz2 table.csv index.csv` to convert the raw
  data into CSVs suitable for importing into CockroachDB and Postgres.

- Start CockroachDB with `--external-io-dir` pointing at the directory with
  table.csv and index.csv.

- To create the necessary tables and fill them in CockroachDB, run:

  `go run *.go crdbload 'postgresql://root@dan.local:26257?sslmode=disable' 'nodelocal:///table.csv' 'nodelocal:///index.csv'`

- To run tests against S2 in CockroachDB, run:

  `go run *.go s2latencies 'postgresql://root@localhost:26257?sslmode=disable' roads.csv.bz2`

- Start Postgres and load the PostGIS extension.

- To create the necessary tables and fill them in Postgres, run:

  `go run *.go pgload 'postgresql://dan@127.0.0.1:5432/postgres?sslmode=disable' path/to/table.csv path/to/index.csv

- To run tests against PostGIS, run:

  `go run *.go pqlatencies 'postgresql://dan@127.0.0.1:5432/postgres?sslmode=disable' roads.csv.bz2`

- To run tests against S2 in Postgres, run:

  `go run *.go s2latencies 'postgresql://dan@127.0.0.1:5432/postgres?sslmode=disable' roads.csv.bz2`

[sample dataset of roads]:https://drive.google.com/file/d/0B1jY75xGiy7ecDEtR1V1X21QVkE/view
[University of Minnesota]: http://spatialhadoop.cs.umn.edu/datasets.html
