#!/bin/sh
# Regenerates internal/geo/au_postcodes.tsv.gz from GeoNames.
#
# GeoNames publishes postal codes under CC BY 4.0, which allows redistribution
# with attribution; see internal/geo/DATA_LICENSE.
set -eu
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/AU.zip" https://download.geonames.org/export/zip/AU.zip
unzip -q -o "$tmp/AU.zip" AU.txt -d "$tmp"
# postcode, place, state, lat, lon, accuracy, council area
awk -F'\t' 'BEGIN{OFS="\t"} $10!="" {print $2,$3,$5,$10,$11,$12,$6}' "$tmp/AU.txt" |
	LC_ALL=C sort -t "$(printf '\t')" -k1,1 -k2,2 |
	gzip -9n >"$(dirname "$0")/../internal/geo/au_postcodes.tsv.gz"
