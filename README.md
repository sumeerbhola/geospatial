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

S2 in CockroachDB

```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8    6228     1000     1.11     9.44    18.87    48.23    60.82     3327     6399    17407    17407
    9    3114     1000     0.46     2.88     8.91    11.53    15.20      927     2687     3967     3967
   10    1557     1000     0.51     1.25     3.67     4.98     7.08      287     1215     1727     1919
   11     778     1000     0.48     0.75     1.51     2.23     6.03       79      383      575      639
   12     389     1000     0.46     0.66     0.98     1.38     6.29       28      151      199      271
   13     194     1000     0.43     0.62     0.79     1.25     2.62       11       57       75      107
   14      97     1000     0.44     0.59     0.75     1.18     2.10        5       24       35       51
   15      48     1000     0.44     0.59     0.72     0.92    46.14        3       10       17       24
   16      24     1000     0.44     0.59     0.72     1.02     1.44        2        5        9       16
   17      12     1000     0.43     0.59     0.72     0.95     6.29        1        4        6        9
   18       6     1000     0.44     0.59     0.72     0.98     1.77        1        3        5        9
   19       3     1000     0.44     0.62     0.79     1.25     2.62        0        2        4        7
   20       1     1000     0.48     0.85     1.18     1.90     6.55        0        2        3        6
```

S2 in CockroachDB without DISTINCT

```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8    6228     1000     0.49     2.10     4.06     9.44    10.49     3199     6143    17407    17407
    9    3114     1000     0.39     0.88     2.03     2.49     8.13      863     2559     3839     3839
   10    1557     1000     0.34     0.59     1.05     1.31     4.98      239     1151     1535     1791
   11     778     1000     0.33     0.49     0.69     0.88     9.44       59      303      495      575
   12     389     1000     0.31     0.48     0.62     0.82     2.23       18      107      159      223
   13     194     1000     0.31     0.46     0.59     0.82     2.62        6       35       49       87
   14      97     1000     0.34     0.46     0.62     0.82     5.24        2       12       19       31
   15      48     1000     0.33     0.46     0.59     0.79     1.51        1        5        9       14
   16      24     1000     0.34     0.46     0.59     0.82     1.44        0        2        4        8
   17      12     1000     0.34     0.46     0.59     0.85     1.57        0        1        2        4
   18       6     1000     0.36     0.46     0.62     0.88     1.64        0        0        2        4
   19       3     1000     0.34     0.48     0.66     0.95    35.65        0        0        1        4
   20       1     1000     0.36     0.59     1.02     1.44    19.92        0        0        0        2
```

PostGIS
```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8    6228     1000     0.39     1.44     5.51    20.97    60.82     3199     6399    17407    17407
    9    3114     1000     0.26     0.69     2.23     8.91    22.02      927     2559     3967     3967
   10    1557     1000     0.26     0.44     0.92     3.80    12.06      247     1151     1599     1791
   11     778     1000     0.22     0.36     0.59     1.77     9.96       59      319      495      575
   12     389     1000     0.21     0.33     0.46     1.64     4.72       18      111      167      223
   13     194     1000     0.20     0.31     0.38     1.11     3.15        6       35       51       87
   14      97     1000     0.20     0.31     0.36     0.69     2.75        2       12       19       33
   15      48     1000     0.20     0.31     0.36     0.44     1.25        1        5        9       14
   16      24     1000     0.20     0.31     0.36     0.43     0.79        0        2        4        8
   17      12     1000     0.20     0.31     0.36     0.41     0.62        0        1        2        4
   18       6     1000     0.21     0.31     0.36     0.43     2.23        0        0        2        4
   19       3     1000     0.21     0.31     0.38     0.48     0.72        0        0        1        4
   20       1     1000     0.22     0.36     0.62     2.88    35.65        0        0        0        2
```

#### Operation: containing

S2 in CockroachDB

```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8    6228     1000     0.56     0.85     1.38     1.97    33.55        0        4        4        8
    9    3114     1000     0.62     0.88     1.44     1.97     9.44        3        6        7       11
   10    1557     1000     0.62     0.92     1.44     2.03     6.55        4        8       11       15
   11     778     1000     0.66     0.95     1.44     1.90    24.12        5       10       13       17
   12     389     1000     0.75     0.95     1.44     1.97    23.07        6       12       15       21
   13     194     1000     0.79     0.98     1.51     2.03    35.65        7       14       17       22
   14      97     1000     0.79     0.98     1.57     2.75    19.92        7       14       18       29
   15      48     1000     0.82     1.02     1.64     2.36     7.34        8       16       19       30
   16      24     1000     0.82     1.05     1.64     2.49     3.93        8       16       20       31
   17      12     1000     0.85     1.05     1.70     2.62     7.86        8       17       21       33
   18       6     1000     0.88     1.11     1.77     2.62    12.58        9       17       21       33
   19       3     1000     0.88     1.11     1.77     2.62     6.03        9       17       22       33
   20       1     1000     0.92     1.25     1.97     3.28     7.86        9       17       22       33
```

PostGIS
```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8    6228     1000     0.16     0.24     0.39     0.69     1.31        0        0        0        0
    9    3114     1000     0.17     0.25     0.41     0.59     1.31        0        1        4        6
   10    1557     1000     0.17     0.26     0.38     0.62     1.64        0        2        4        7
   11     778     1000     0.17     0.28     0.39     0.75     1.44        0        4        6        8
   12     389     1000     0.18     0.28     0.43     0.66     1.11        1        5        6        8
   13     194     1000     0.16     0.29     0.43     0.59     1.02        2        6        7        9
   14      97     1000     0.18     0.29     0.44     0.66     1.18        2        6        8       10
   15      48     1000     0.20     0.29     0.44     0.66     1.38        3        7       10       13
   16      24     1000     0.19     0.29     0.43     0.69     1.38        3        8       10       14
   17      12     1000     0.19     0.29     0.46     0.79     1.25        3        8       10       14
   18       6     1000     0.19     0.29     0.48     0.79     1.25        3        8       11       14
   19       3     1000     0.20     0.31     0.49     0.92     1.57        3        8       11       14
   20       1     1000     0.20     0.33     0.66     1.31     2.36        3        8       11       14
```


#### Operation: intersects

S2 in CockroachDB

```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8    6228     1000     1.84    10.49    23.07    52.43    67.11     3327     6399    17407    17407
    9    3114     1000     0.92     3.41     9.44    12.58    20.97      927     2687     3967     3967
   10    1557     1000     0.85     1.70     4.19     6.03    15.20      287     1215     1727     1919
   11     778     1000     0.75     1.25     2.23     2.75     5.77       83      383      575      639
   12     389     1000     0.75     1.11     1.57     2.23     3.67       33      159      215      271
   13     194     1000     0.75     1.02     1.44     2.49     4.72       18       67       83      119
   14      97     1000     0.79     1.02     1.51     2.49    31.46       13       35       45       55
   15      48     1000     0.82     1.02     1.51     3.01    10.49       11       22       28       37
   16      24     1000     0.82     1.05     1.57     2.36    11.53       10       19       24       37
   17      12     1000     0.82     1.05     1.64     2.62     4.06        9       18       23       35
   18       6     1000     0.85     1.11     1.70     3.01    46.14        9       18       23       35
   19       3     1000     0.88     1.18     1.70     3.15     5.77        9       18       23       35
   20       1     1000     0.95     1.51     1.97     3.41     7.86        9       18       23       35
```

PostGIS
```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8    6228     1000     0.48     1.64     3.54     5.51     7.60     3327     6655    17407    17407
    9    3114     1000     0.36     0.79     1.38     1.90     3.28      991     2687     4095     4095
   10    1557     1000     0.34     0.56     0.88     1.11     1.77      287     1215     1727     1919
   11     778     1000     0.31     0.46     0.62     0.82     2.75       87      399      607      639
   12     389     1000     0.29     0.43     0.56     0.66     0.92       35      159      207      271
   13     194     1000     0.29     0.41     0.49     0.59     0.88       17       63       83      119
   14      97     1000     0.28     0.41     0.48     0.59     1.11       10       30       41       57
   15      48     1000     0.29     0.41     0.48     0.59     0.88        8       18       24       30
   16      24     1000     0.29     0.41     0.48     0.56     0.92        7       13       17       24
   17      12     1000     0.28     0.39     0.48     0.56     1.18        6       12       15       24
   18       6     1000     0.29     0.41     0.48     0.59     1.02        6       11       15       24
   19       3     1000     0.29     0.41     0.51     0.59     1.18        6       11       15       21
   20       1     1000     0.33     0.46     0.69     0.79     1.18        6       11       15       17
```

## Shape: cap

- Note that a huge number more results are being returned from CockroachDB.
  TODO(dan): This effect persists even if a very large number of cells (100) are
  used in the cap covering. I think that means my test code has a bug and is not
  correctly creating the same caps, which is believable. Revisit.

#### Operation: contains

S2 in CockroachDB

```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8    6228      100    52.43   113.25   201.33   201.33   209.72    40959    69631    69631    69631
    9    3114      100    11.01    35.65    75.50    79.69   109.05    12287    27647    28671    40959
   10    1557      100     1.38    11.53    26.21    48.23    48.23     4095     8703    18431    18431
   11     778      100     0.82     3.93     9.44    11.01    12.58     1343     3455     3967     4095
   12     389      100     0.79     1.64     4.06     4.46     5.24      367     1343     1471     1791
   13     194      100     0.66     1.05     1.77     2.36     2.49      115      415      543      671
   14      97      100     0.59     0.82     1.18     1.57     2.03       41      151      303      319
   15      48      100     0.59     0.79     0.98     1.05     1.84       13       51       67       75
   16      24      100     0.56     0.72     0.88     1.02     1.25        6       24       39       43
   17      12      100     0.56     0.72     0.88     1.05     1.11        3       11       13       15
   18       6      100     0.56     0.72     0.92     1.11     1.57        1        5        7        8
   19       3      100     0.59     0.75     0.95     1.11     1.57        1        3        6        6
   20       1      100     0.88     1.11     1.57     1.84     6.55        0        3        4        6
```

PostGIS
```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8    6228     1000     0.59     0.92     1.44     2.62     6.82      511     1791     2303     2815
    9    3114     1000     0.59     0.75     0.95     1.31     3.15      135      639      831      991
   10    1557     1000     0.56     0.69     0.79     1.02     2.03       39      215      287      367
   11     778     1000     0.52     0.66     0.75     0.92     1.70       12       67       91      151
   12     389     1000     0.52     0.66     0.72     0.92     1.57        4       22       30       43
   13     194     1000     0.52     0.62     0.72     0.92     1.38        1        8       14       23
   14      97     1000     0.52     0.62     0.69     0.98     1.70        0        3        6       11
   15      48     1000     0.52     0.62     0.72     1.02     1.97        0        2        3        6
   16      24     1000     0.52     0.62     0.72     0.95     1.77        0        1        2        5
   17      12     1000     0.52     0.62     0.72     0.95     1.64        0        0        1        4
   18       6     1000     0.52     0.62     0.72     0.92     1.44        0        0        0        3
   19       3     1000     0.51     0.62     0.75     1.11     2.03        0        0        0        1
   20       1     1000     0.52     0.66     0.98     1.70    33.55        0        0        0        0
```

## Shape: rect

- Note that PostGIS is able to query using arbitrary bounding boxes, whereas S2
  can only query shapes built out of a union of cells. In this test, this
  results in S2 generating an absurd covering that pulls back about 3X the
  surface area that PostGIS does.

#### Operation: contains

S2 in CockroachDB

```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8    6228      100    75.50   117.44   201.33   226.49   234.88    36863    61439    61439    69631
    9    3114      100    14.16    31.46    79.69    83.89   109.05    10239    25599    26623    26623
   10    1557      100     1.97    10.49    16.78    27.26    33.55     3455     5375     5887    12287
   11     778      100     1.05     3.93     8.13     9.44     9.96     1215     2559     2943     2943
   12     389      100     0.72     1.84     3.54     4.19     6.82      351      991     1087     1087
   13     194      100     0.69     1.11     2.03     3.41     4.98       99      399      431      463
   14      97      100     0.62     0.88     1.64     2.23     2.62       35      123      151      207
   15      48      100     0.62     0.82     1.57     1.97     2.49       12       45       63       67
   16      24      100     0.62     0.79     1.38     2.10     2.23        5       19       28       37
   17      12      100     0.59     0.82     1.25     1.44     2.75        3        9       14       14
   18       6      100     0.59     0.79     1.11     1.90     3.01        1        4        8        8
   19       3      100     0.66     0.85     1.44     2.36     2.88        1        3        6        6
   20       1      100     0.92     1.18     1.70     3.41     5.77        0        2        4        6
```

PostGIS
```
level__meters_____numQ_pMin(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__count50__count95__count99_countMax
    8    6228     1000     0.38     1.44     3.15     5.51     8.91     3199     6399    17407    17407
    9    3114     1000     0.26     0.69     1.31     1.90     6.03      927     2559     3967     3967
   10    1557     1000     0.26     0.44     0.79     0.98     1.70      247     1151     1599     1791
   11     778     1000     0.24     0.38     0.52     0.69     1.44       59      319      495      575
   12     389     1000     0.23     0.34     0.44     0.59     1.05       18      111      167      223
   13     194     1000     0.22     0.33     0.39     0.59     4.19        6       35       51       87
   14      97     1000     0.21     0.31     0.39     0.59     1.44        2       12       19       33
   15      48     1000     0.22     0.31     0.38     0.51     1.90        1        5        9       14
   16      24     1000     0.21     0.31     0.38     0.52     2.23        0        2        4        8
   17      12     1000     0.21     0.31     0.39     0.51     2.75        0        1        2        4
   18       6     1000     0.21     0.31     0.38     0.56     2.88        0        0        2        4
   19       3     1000     0.21     0.31     0.43     0.72     4.06        0        0        1        4
   20       1     1000     0.24     0.36     0.59     0.98    24.12        0        0        0        2
```


# Reproduction Steps

- Download the [sample dataset of roads] courtesy of [University of Minnesota].

- Run `go run *.go convert roads.csv.bz2 table.csv index.csv` to convert the raw
  data into CSVs suitable for importing into CockroachDB and Postgres.

- Start CockroachDB with `--external-io-dir` pointing at the directory with
  table.csv and index.csv.

- To create the necessary tables and fill them in CockroachDB, run:

  `go run *.go crdbload 'postgresql://root@dan.local:26257?sslmode=disable' 'nodelocal:///table.csv' 'nodelocal:///index.csv'`

- To run tests against CockroachDB, run:

  `go run *.go crdbquery 'postgresql://root@localhost:26257?sslmode=disable' roads.csv.bz2`

- Start Postgres and load the PostGIS extension.

- To create the necessary tables and fill them in Postgres, run:

  `go run *.go pgload 'postgresql://dan@127.0.0.1:5432/postgres?sslmode=disable' path/to/table.csv

- To run tests against Postgres, run:

  `go run *.go pqquery 'postgresql://dan@127.0.0.1:5432/postgres?sslmode=disable' roads.csv.bz2`

[sample dataset of roads]:https://drive.google.com/file/d/0B1jY75xGiy7ecDEtR1V1X21QVkE/view
[University of Minnesota]: http://spatialhadoop.cs.umn.edu/datasets.html
