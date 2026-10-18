#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Draw the event-flow trees under events/ as figures.

Each events/<operator>.txt holds the trees in the notation the pages once
drew them in: `├── name` opens a sub-reconciler, `├─ condition → Type Reason`
is an event, a line without a marker continues the event above.
"""
import pathlib
import re
import sys
from tree_fig import parse_tree, tree_figure, alt_text

HERE = pathlib.Path(__file__).resolve().parent
OUT = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 else pathlib.Path(__file__).resolve().parents[2] / "docs" / "diagrams"

PAGES = {
    "service-barbican-events": ("barbican", "Event flow of the Barbican operator",
                                "The event flow of the Barbican operator: the two reconcilers, the sub-reconcilers that emit events, and for each the condition and the event it emits."),
    "service-cinder-events": ("cinder", "Event flow of the Cinder operator",
                              "The event flow of the Cinder operator: the two reconcilers, the sub-reconcilers that emit events, and for each the condition and the event it emits."),
    "service-glance-events": ("glance", "Event flow of the Glance operator",
                              "The event flow of the Glance operator: the sub-reconcilers that emit events, and for each the condition and the event it emits."),
    "service-keystone-events": ("keystone", "Event flow of the Keystone operator",
                                "The event flow of the Keystone operator: the sub-reconcilers that emit events, and for each the condition and the event it emits."),
    "service-neutron-events": ("neutron", "Event flow of the Neutron operator",
                               "The event flow of the Neutron operator: the two reconcilers, the sub-reconcilers that emit events, and for each the condition and the event it emits."),
    "compute-nova-events": ("nova", "Event flow of the Nova operator",
                            "The event flow of the Nova operator: the two reconcilers, the sub-reconcilers that emit events, and for each the condition and the event it emits."),
    "compute-ovn-events": ("ovn", "Event flow of the OVN operator",
                           "The event flow of the OVN operator: the two reconcilers, the sub-reconcilers that emit events, and for each the condition and the event it emits."),
    "service-placement-events": ("placement", "Event flow of the Placement operator",
                                 "The event flow of the Placement operator: the sub-reconcilers that emit events, and for each the condition and the event it emits."),
}

FOOTER = ["A filled chip is a Warning event, an outline chip a Normal event. A line with two chips emits one or the other.",
          "Every reason is described in the Event Reasons Reference of the page."]


def split_trees(block):
    trees, cur = [], []
    for line in block.split("\n"):
        if re.match(r"^[A-Za-z]", line):  # a root line
            if cur:
                trees.append(cur)
            cur = [line]
        else:
            cur.append(line)
    if cur:
        trees.append(cur)
    return trees


for fid, (operator, name, intro) in PAGES.items():
    block = (HERE / "events" / f"{operator}.txt").read_text(encoding="utf-8").rstrip("\n")
    trees = [parse_tree(t) for t in split_trees(block)]
    fig = tree_figure(fid, name, trees, footer=FOOTER)
    fig.write(str(OUT / fid))
    if "--alt" in sys.argv:
        print(fid, alt_text(intro, trees))
    else:
        print(fid, fig.w, fig.h)
