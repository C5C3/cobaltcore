# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# anchor-error-substrings.awk — the V3 helper of audit-validation-parity.sh.
# Portable to BSD awk (one-true-awk): no gawk extensions, no match() arrays,
# no regex interval braces. Three modes, selected with -v mode=…:
#
#   go      Read Go sources. Print one record per string literal, with
#           "+"-concatenated literals joined and \" \\ \' unescaped:
#             S <TAB> surface <TAB> text
#           and one record per "// +marker" comment line:
#             K <TAB> surface <TAB> comment text
#           surface is operators/<op>/api or internal/common.
#   crd     Read generated CRD YAMLs. Print one record per key directly under
#           a properties: mapping and per schema boundary keyword:
#             P <TAB> op <TAB> kind <TAB> property
#             B <TAB> op <TAB> kind <TAB> keyword <TAB> value
#   anchor  Classify every asserted $error substring of one suite. Arguments:
#           the suite's numbered fixtures, then the crd record file (-v
#           crdrec=), the go record file (-v gorec=), and last the substring
#           list (-v subs=). -v op= names the operator; -v surfaces= lists the
#           Go surfaces its webhooks reach. Prints, per substring:
#             <category> <TAB> <substring> [<TAB> reason]
#           category is one of go, template, schema-phrase, server-phrase,
#           field-path, fixture — or stale-boundary / none for a miss.

# ---------------------------------------------------------------------------
# mode=go
# ---------------------------------------------------------------------------
function go_flush() {
  if (have_lit) print "S\t" surface "\t" cur
  cur = ""; have_lit = 0; joining = 0
}

# go_unescape — undo the escapes a chainsaw substring can quote verbatim
# (\" \\ \'); every other escape sequence is kept as written.
function go_unescape(s,    out, i, n, c) {
  if (index(s, "\\") == 0) return s
  out = ""; n = length(s)
  for (i = 1; i <= n; i++) {
    c = substr(s, i, 1)
    if (c == "\\" && i < n) {
      i++
      c = substr(s, i, 1)
      if (c != "\"" && c != "\\" && c != "'") c = "\\" c
    }
    out = out c
  }
  return out
}

mode == "go" && FNR == 1 {
  go_flush()
  surface = FILENAME
  if (match(surface, /operators\/[^\/]+\/api/)) surface = substr(surface, RSTART, RLENGTH)
  else if (match(surface, /internal\/common/)) surface = "internal/common"
  in_blk = 0; in_raw = 0
}

mode == "go" {
  rest = $0
  if (in_raw) {
    p = index(rest, "`")
    if (p == 0) { print "S\t" surface "\t" rest; next }
    print "S\t" surface "\t" substr(rest, 1, p - 1)
    rest = substr(rest, p + 1); in_raw = 0
  }
  if (in_blk) {
    p = index(rest, "*/")
    if (p == 0) next
    rest = substr(rest, p + 2); in_blk = 0
  }
  # A literal that ended the previous line with "+" joins the first literal
  # of this line; any other token ends the concatenation.
  if (joining && rest !~ /^[ \t]*["`]/) go_flush()
  while (match(rest, /"|`|'|\/\/|\/\*/)) {
    t2 = substr(rest, RSTART, 2)
    t1 = substr(rest, RSTART, 1)
    if (t2 == "//") {
      cmt = substr(rest, RSTART + 2)
      if (cmt ~ /^[ \t]*\+/) print "K\t" surface "\t" cmt
      break
    }
    if (t2 == "/*") {
      rest = substr(rest, RSTART + 2)
      p = index(rest, "*/")
      if (p == 0) { in_blk = 1; break }
      rest = substr(rest, p + 2)
      continue
    }
    r = substr(rest, RSTART)
    if (t1 == "'") {
      if (match(r, /^'([^'\\]|\\.)*'/)) rest = substr(r, RLENGTH + 1)
      else rest = substr(r, 2)
      continue
    }
    if (t1 == "\"") {
      if (!match(r, /^"([^"\\]|\\.)*"/)) break
      lit = go_unescape(substr(r, 2, RLENGTH - 2))
      rest = substr(r, RLENGTH + 1)
    } else {
      p = index(substr(r, 2), "`")
      if (p == 0) {
        # Raw string spanning lines: one record per line, no joining.
        go_flush()
        print "S\t" surface "\t" substr(r, 2)
        in_raw = 1
        break
      }
      lit = substr(r, 2, p - 1)
      rest = substr(r, p + 2)
    }
    cur = cur lit; have_lit = 1
    if (rest ~ /^[ \t]*\+[ \t]*["`]/) { sub(/^[ \t]*\+[ \t]*/, "", rest); continue }
    if (rest ~ /^[ \t]*\+[ \t]*(\/\/.*)?$/) { joining = 1; break }
    go_flush()
  }
}

# ---------------------------------------------------------------------------
# mode=crd
# ---------------------------------------------------------------------------
function yaml_scalar(v) {
  sub(/[ \t]+$/, "", v)
  if (v ~ /^'.*'$/) { v = substr(v, 2, length(v) - 2); gsub(/''/, "'", v) }
  else if (v ~ /^".*"$/) { v = substr(v, 2, length(v) - 2); gsub(/\\\\/, "\\", v); gsub(/\\"/, "\"", v) }
  return v
}

mode == "crd" && FNR == 1 {
  crd_op = FILENAME
  sub(/^operators\//, "", crd_op); sub(/\/.*$/, "", crd_op)
  crd_kind = ""; nst = 0
}

mode == "crd" {
  # spec.names.kind is the first "kind:" at four spaces of indentation.
  if (crd_kind == "" && $0 ~ /^    kind: /) crd_kind = $2
  match($0, /^ */)
  ind = RLENGTH
  body = substr($0, ind + 1)
  if (body == "") next
  while (nst > 0 && ind <= st[nst]) nst--
  if (nst > 0 && ind == st[nst] + 2 && body ~ /^[A-Za-z0-9_"'$.-][^:]*:([ \t]|$)/) {
    key = body; sub(/:.*$/, "", key); gsub(/["']/, "", key)
    print "P\t" crd_op "\t" crd_kind "\t" key
  }
  if (body ~ /^properties:[ \t]*$/) {
    st[++nst] = ind
  } else if (body ~ /^(minimum|maximum|exclusiveMinimum|exclusiveMaximum|minLength|maxLength|minItems|maxItems|minProperties|maxProperties|multipleOf|uniqueItems|pattern):[ \t]+[^ \t]/) {
    key = body; sub(/:.*$/, "", key)
    val = body; sub(/^[^:]*:[ \t]+/, "", val)
    print "B\t" crd_op "\t" crd_kind "\t" key "\t" yaml_scalar(val)
  } else if (body ~ /^enum:/) {
    print "B\t" crd_op "\t" crd_kind "\tenum\tany"
  }
}

# ---------------------------------------------------------------------------
# mode=anchor
# ---------------------------------------------------------------------------
function norm_num(v) {
  if (v ~ /^-?[0-9]+(\.[0-9]+)?$/) return sprintf("%.10g", v + 0)
  return v
}

# hay_has — s occurs in a Go literal / marker (which == "go") or in either
# that or a fixture (which == "any"). The texts are held in ~8 KiB chunks
# joined by newlines, so a single-line substring cannot straddle two records.
function hay_has(s, which,    i) {
  for (i = 1; i <= ngo; i++) if (index(gochunk[i], s)) return 1
  if (which == "any") for (i = 1; i <= nfix; i++) if (index(fixchunk[i], s)) return 1
  return 0
}

function kind_selected(k) { return (k in selkind) }

# has_bound — a marker or generated-CRD boundary keyword with value v exists
# (v == "" accepts any value of the keyword).
function has_bound(kw, v,    key, parts) {
  for (key in cbound) {
    split(key, parts, SUBSEP)
    if (parts[2] != kw || !kind_selected(parts[1])) continue
    if (v == "" || parts[3] == v) return 1
  }
  for (key in gbound) {
    split(key, parts, SUBSEP)
    if (parts[1] != kw) continue
    if (v == "" || parts[2] == v) return 1
  }
  return 0
}

function has_pattern(rx, exact,    key, parts) {
  for (key in cbound) {
    split(key, parts, SUBSEP)
    if (parts[2] != "pattern" || !kind_selected(parts[1])) continue
    if (exact ? parts[3] == rx : index(parts[3], rx) == 1) return 1
  }
  for (key in gbound) {
    split(key, parts, SUBSEP)
    if (parts[1] != "pattern") continue
    if (exact ? parts[2] == rx : index(parts[2], rx) == 1) return 1
  }
  return 0
}

# schema_check — "" when s is not a go-openapi / apiextensions schema phrase;
# "ok" when the boundary it names is carried by a marker or the CRD; otherwise
# the stale-boundary reason.
function schema_check(s,    p, n, kw, rx) {
  p = s
  sub(/^in body /, "", p)
  if (p == "should match") return has_bound("pattern", "") ? "ok" : "no pattern marker or CRD pattern exists"
  if (index(p, "should match '") == 1) {
    rx = substr(p, 15)
    if (rx ~ /'$/) return has_pattern(substr(rx, 1, length(rx) - 1), 1) ? "ok" : "no marker or CRD pattern equals the quoted regex"
    return has_pattern(rx, 0) ? "ok" : "no marker or CRD pattern starts with the quoted regex"
  }
  if (p == "shouldn't contain duplicates") return has_bound("uniqueItems", "true") ? "ok" : "no uniqueItems marker or CRD value"
  if (p ~ /^should be at least -?[0-9][0-9.]* chars long$/) kw = "minLength"
  else if (p ~ /^should be at most -?[0-9][0-9.]* chars long$/) kw = "maxLength"
  else if (p ~ /^may not be more than [0-9]+ (byte|bytes|character|characters)$/) kw = "maxLength"
  else if (p ~ /^should be greater than or equal to -?[0-9][0-9.]*$/) kw = "minimum"
  else if (p ~ /^should be greater than -?[0-9][0-9.]*$/) kw = "minimum!"
  else if (p ~ /^should be less than or equal to -?[0-9][0-9.]*$/) kw = "maximum"
  else if (p ~ /^should be less than -?[0-9][0-9.]*$/) kw = "maximum!"
  else if (p ~ /^should have at least [0-9]+ items$/) kw = "minItems"
  else if (p ~ /^should have at most [0-9]+ items$/) kw = "maxItems"
  else if (p ~ /^must have at most [0-9]+ (item|items)$/) kw = "maxItems|maxProperties"
  else if (p ~ /^should have at least [0-9]+ properties$/) kw = "minProperties"
  else if (p ~ /^should have at most [0-9]+ properties$/) kw = "maxProperties"
  else if (p ~ /^should be a multiple of -?[0-9][0-9.]*$/) kw = "multipleOf"
  else return ""
  n = p
  sub(/^[^0-9-]*/, "", n); sub(/[^0-9.]*$/, "", n)
  n = norm_num(n)
  if (kw == "minimum!") {
    if (has_bound("minimum", n) && has_bound("exclusiveMinimum", "true")) return "ok"
    return "boundary exclusive minimum " n " is carried by no marker or CRD value"
  }
  if (kw == "maximum!") {
    if (has_bound("maximum", n) && has_bound("exclusiveMaximum", "true")) return "ok"
    return "boundary exclusive maximum " n " is carried by no marker or CRD value"
  }
  if (kw == "maxItems|maxProperties") {
    if (has_bound("maxItems", n) || has_bound("maxProperties", n)) return "ok"
    return "boundary maxItems/maxProperties " n " is carried by no marker or CRD value"
  }
  if (has_bound(kw, n)) return "ok"
  return "boundary " kw "=" n " is carried by no marker or CRD value"
}

# server_phrase — the field.ErrorType strings apimachinery renders (and their
# first word), plus the prefix the API server puts on a webhook denial.
function server_phrase(s,    e) {
  if (s in errtype) return 1
  if (length(s) >= 5) for (e in errtype) if (index(e " ", s " ") == 1) return 1
  # field.NotSupported renders its detail as `supported values: "a", "b"`.
  return (s == "admission webhook" || s == "denied the request" || s == "supported values")
}

function is_crd_prop(seg,    k) {
  for (k in selkind) if ((k, seg) in cprop) return 1
  return 0
}

# field_path — a server-rendered path (dotted and/or indexed): every segment,
# its [N] / [key] index stripped, is a json tag of the Go corpus, a property of
# the suite kind's CRD (which covers embedded Kubernetes types), or a key of a
# sibling fixture (map keys such as memory).
function field_path(s,    n, segs, i, seg) {
  if (s !~ /^[A-Za-z_][A-Za-z0-9_-]*(\[[A-Za-z0-9_.:\/-]*\])*(\.[A-Za-z_][A-Za-z0-9_-]*(\[[A-Za-z0-9_.:\/-]*\])*)*$/) return 0
  if (index(s, ".") == 0 && index(s, "[") == 0) return 0
  n = split(s, segs, ".")
  for (i = 1; i <= n; i++) {
    seg = segs[i]
    sub(/\[.*$/, "", seg)
    if (i == 1 && (seg == "spec" || seg == "metadata" || seg == "status")) continue
    if (!(seg in jsontag) && !is_crd_prop(seg) && !(seg in fixkey)) return 0
  }
  return 1
}

function nonspace(s,    x) { x = s; return gsub(/[^ \t]/, "", x) }
function nwords(s,    x) { x = s; return gsub(/[^ \t]+/, "", x) }

# load_template — split template literal t on its fmt verbs into the literal
# fragments TF[0..TN] around TN verbs; TC[k] is the value class of verb k, TM[k]
# whether the class is a character run (a failing value cannot grow into a
# passing one), TQ[k] whether the value must also occur in the Go corpus or a
# fixture (every non-numeric verb: %s %v %q …).
function load_template(t,    frag, v, c) {
  TN = 0; frag = ""
  while (match(t, /%[-+# 0]*[0-9*]*(\.[0-9*]+)?[A-Za-z%]/)) {
    v = substr(t, RSTART, RLENGTH)
    frag = frag substr(t, 1, RSTART - 1)
    t = substr(t, RSTART + RLENGTH)
    if (v == "%%") { frag = frag "%"; continue }
    TF[TN] = frag; frag = ""
    TN++
    c = substr(v, length(v), 1)
    if (c ~ /[dboOcU]/)       { TC[TN] = "^-?[0-9]+$"; TM[TN] = 1; TQ[TN] = 0 }
    else if (c ~ /[xX]/)      { TC[TN] = "^[0-9a-fA-Fx]+$"; TM[TN] = 1; TQ[TN] = 0 }
    else if (c ~ /[eEfFgG]/)  { TC[TN] = "^-?[0-9][0-9.eE+-]*$"; TM[TN] = 1; TQ[TN] = 0 }
    else if (c == "q")        { TC[TN] = "^\"?[^\"]*\"?$"; TM[TN] = 0; TQ[TN] = 1 }
    else                      { TC[TN] = "^[^ \t]+$"; TM[TN] = 1; TQ[TN] = 1 }
  }
  TF[TN] = frag t
}

# tmatch_verb — r starts with a value of verb k followed by fragment TF[k]
# (or, where r runs out, a prefix of either); lit and lw count the literal
# non-space characters and words matched so far. A match needs MINLIT
# characters over at least two words, so "43 characters" alone never anchors.
function tmatch_verb(r, k, lit, lw,    lr, L, val, q, rem, f, nf, lrem) {
  lr = length(r)
  for (L = 1; L <= lr; L++) {
    val = substr(r, 1, L)
    if (val !~ TC[k]) { if (TM[k]) break; continue }
    # A %s/%v/%q value must occur in the corpus or a fixture, unless it
    # starts with a digit: a rendered number, quantity, duration, or release
    # (1Mi, 1m0s, 2025.2) is taken like a %d value.
    if (TQ[k]) {
      q = val; gsub(/"/, "", q)
      if (q != "" && q !~ /^[0-9]/ && !hay_has(q, "any")) { if (TM[k]) break; continue }
    }
    rem = substr(r, L + 1)
    if (rem == "") return (lit >= MINLIT && lw >= 2)
    f = TF[k]; nf = length(f); lrem = length(rem)
    if (lrem <= nf) {
      if (substr(f, 1, lrem) == rem && lit + nonspace(rem) >= MINLIT && lw + nwords(rem) >= 2) return 1
      continue
    }
    if (k < TN && substr(rem, 1, nf) == f)
      if (tmatch_verb(substr(rem, nf + 1), k + 1, lit + nonspace(f), lw + nwords(f))) return 1
  }
  return 0
}

# tmatch — s is a contiguous piece of some rendering of the loaded template
# that spans at least one verb: s = (suffix of TF[i]) value TF[i+1] … (prefix
# of TF[j]).
function tmatch(s,    i, f, nf, ls, L) {
  ls = length(s)
  for (i = 0; i < TN; i++) {
    f = TF[i]; nf = length(f)
    for (L = (nf < ls ? nf : ls); L >= 0; L--) {
      if (L == ls) continue
      if (substr(s, 1, L) != substr(f, nf - L + 1)) continue
      if (tmatch_verb(substr(s, L + 1), i + 1, nonspace(substr(s, 1, L)), nwords(substr(s, 1, L)))) return 1
    }
  }
  return 0
}

function template_anchor(s,    n, nw, words, i, j, ok, w) {
  nw = 0
  n = split(s, words, /[^A-Za-z0-9_]+/)
  for (i = 1; i <= n; i++) if (length(words[i]) >= 4) w[++nw] = words[i]
  if (nw == 0) return 0
  for (j = 1; j <= ntmpl; j++) {
    ok = 0
    for (i = 1; i <= nw; i++) if (index(tmpl[j], w[i])) { ok = 1; break }
    if (!ok) continue
    load_template(tmpl[j])
    if (TN > 0 && tmatch(s)) return 1
  }
  return 0
}

# A numbered fixture of the suite. YAML comments are dropped first: a header
# comment that restates the rule's message is not a value the server echoes.
mode == "anchor" && FILENAME != crdrec && FILENAME != gorec && FILENAME != subs {
  line = $0
  sub(/(^|[ \t])#.*$/, "", line)
  if (line ~ /^kind:[ \t]/) { split(line, kv, /[ \t]+/); fixkind[kv[2]] = 1 }
  if (match(line, /^[ \t]*(- )?[A-Za-z0-9_.\/-]+:([ \t]|$)/)) {
    key = substr(line, RSTART, RLENGTH)
    sub(/^[ \t]*(- )?/, "", key); sub(/:.*$/, "", key)
    fixkey[key] = 1
  }
  if (line == "") next
  fixbuf = fixbuf "\n" line
  if (length(fixbuf) > 8192) { fixchunk[++nfix] = fixbuf; fixbuf = "" }
  next
}

mode == "anchor" && FILENAME == crdrec {
  split($0, f, "\t")
  if (f[1] == "P") { cprop[f[3], f[4]] = 1; crdkind[f[3]] = f[2] }
  else if (f[1] == "B") { cbound[f[3], f[4], norm_num(f[5])] = 1; crdkind[f[3]] = f[2] }
  next
}

mode == "anchor" && FILENAME == gorec {
  t = index($0, "\t"); typ = substr($0, 1, t - 1); rest = substr($0, t + 1)
  t = index(rest, "\t"); surf = substr(rest, 1, t - 1); text = substr(rest, t + 1)
  if (!(surf in want)) next
  gobuf = gobuf "\n" text
  if (length(gobuf) > 8192) { gochunk[++ngo] = gobuf; gobuf = "" }
  if (typ == "S") {
    if (match(text, /json:"[^",]+/)) jsontag[substr(text, RSTART + 6, RLENGTH - 6)] = 1
    if (text ~ /%[-+# 0]*[0-9*]*(\.[0-9*]+)?[A-Za-z]/) tmpl[++ntmpl] = text
  } else if (match(text, /validation:(items:)?(Minimum|Maximum|ExclusiveMinimum|ExclusiveMaximum|MinLength|MaxLength|MinItems|MaxItems|MinProperties|MaxProperties|MultipleOf|UniqueItems|Pattern|Enum)=/)) {
    kw = substr(text, RSTART, RLENGTH - 1)
    sub(/^validation:(items:)?/, "", kw)
    kw = tolower(substr(kw, 1, 1)) substr(kw, 2)
    val = substr(text, RSTART + RLENGTH)
    sub(/[ \t]+$/, "", val)
    if (val ~ /^`.*`$/ || val ~ /^".*"$/) val = substr(val, 2, length(val) - 2)
    if (kw == "enum") val = "any"
    gbound[kw, norm_num(val)] = 1
  }
  next
}

mode == "anchor" && FILENAME == subs {
  if ($0 != "") sublist[++nsub] = $0
  next
}

BEGIN {
  MINLIT = 8
  if (mode == "anchor") {
    ns = split(surfaces, sv, " ")
    for (i = 1; i <= ns; i++) want[sv[i]] = 1
    split("Not found|Required value|Duplicate value|Invalid value|Unsupported value|Forbidden|Too long|Too many|Too few|Internal error|Too short", ev, "|")
    for (i in ev) errtype[ev[i]] = 1
  }
}

END {
  if (mode == "go") go_flush()
  if (mode != "anchor") exit
  if (fixbuf != "") fixchunk[++nfix] = fixbuf
  if (gobuf != "") gochunk[++ngo] = gobuf
  # The suite's kinds pick the CRDs whose properties and boundaries count;
  # fall back to every CRD of the operator when no fixture kind is a CRD.
  for (k in fixkind) if (k in crdkind) selkind[k] = 1
  nsel = 0
  for (k in selkind) nsel++
  if (nsel == 0) for (k in crdkind) if (crdkind[k] == op) selkind[k] = 1

  for (i = 1; i <= nsub; i++) {
    s = sublist[i]
    if (server_phrase(s)) { print "server-phrase\t" s; continue }
    sc = schema_check(s)
    if (sc == "ok") { print "schema-phrase\t" s; continue }
    if (hay_has(s, "go")) { print "go\t" s; continue }
    if (sc != "") { print "stale-boundary\t" s "\t" sc; continue }
    if (field_path(s)) { print "field-path\t" s; continue }
    if (fixture_has(s)) { print "fixture\t" s; continue }
    if (template_anchor(s)) { print "template\t" s; continue }
    print "none\t" s "\tno Go literal or marker, fmt template, schema/server phrase, field path, or fixture value carries it"
  }
}

function fixture_has(s,    i) {
  for (i = 1; i <= nfix; i++) if (index(fixchunk[i], s)) return 1
  return 0
}
