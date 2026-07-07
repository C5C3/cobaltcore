#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Generate values.schema.json for the operator Helm charts from one source.

The keystone-operator and c5c3-operator charts share the bulk of their Helm
values schema: the resourceQuantity definition, the image / replicas /
resources / rbac / leaderElection / webhook / metrics / logging / monitoring /
serviceAccount / name-override properties, the operator-library subchart-values
property, and the rbac->webhook constraint. This script holds that shared
schema once and emits each chart's values.schema.json, layering the
chart-specific pieces on top: the image repository default, the webhook
description, and -- for keystone only -- the cidr/stringMap definitions plus the
NetworkPolicy property and its fail-closed constraint.

Editing a shared field here regenerates both charts, so the two schemas cannot
drift. Run via the Makefile:

  make gen-helm-schema     # write both values.schema.json files
  make verify-helm-schema  # exit non-zero if either committed file is stale
"""

import argparse
import json
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent

# --- Shared definitions -----------------------------------------------------

RESOURCE_QUANTITY = {
    "description": "Kubernetes resource quantity (e.g., '500m', '128Mi', '1Gi', '0.5', '1e3')",
    "anyOf": [
        {
            "type": "string",
            "pattern": r"^(\.[0-9]+|[0-9]+(\.[0-9]*)?)((e[0-9]+)|(m|k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei))?$",
        },
        {"type": "number", "minimum": 0},
    ],
}

# --- keystone-only definitions ----------------------------------------------

CIDR = {
    "description": "IPv4 or IPv6 CIDR block (e.g., '10.96.0.1/32', '2001:db8::/32'). IPv4 octets are bounded 0-255 to surface obvious typos (e.g. '999.0.0.1/32') at helm render time rather than as opaque kubectl apply admission errors. The IPv6 half of the pattern is intentionally loose — it matches any hex+colon sequence and does not enforce valid RFC 4291 group counts or '::' rules — because a schema-complete IPv6 regex is disproportionately complex for the stated goal of catching obvious typos; final IPv6 validation is delegated to kubectl/apiserver admission of the rendered NetworkPolicy.",
    "type": "string",
    "pattern": r"^(((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)/([0-9]|[12][0-9]|3[0-2])|[0-9a-fA-F:]+/([0-9]|[1-9][0-9]|1[01][0-9]|12[0-8]))$",
}

STRING_MAP = {
    "description": "Map of string keys to string values (Kubernetes labels/selectors)",
    "type": "object",
    "additionalProperties": {"type": "string"},
}

# --- Shared property blocks -------------------------------------------------


def image_property(repository_default):
    return {
        "type": "object",
        "description": "Container image configuration for the operator",
        "additionalProperties": False,
        "properties": {
            "repository": {
                "type": "string",
                "description": "Container image repository",
                "default": repository_default,
            },
            "tag": {
                "type": "string",
                "description": "Container image tag; defaults to .Chart.AppVersion when left empty",
                "default": "",
            },
            "digest": {
                "type": "string",
                "description": (
                    "Optional immutable image digest; when set, the container image"
                    " is rendered as repository:tag@digest so the digest (not the"
                    " mutable tag) selects the image"
                ),
                "default": "",
                "pattern": "^(sha256:[a-f0-9]{64})?$",
            },
            "pullPolicy": {
                "type": "string",
                "description": "Image pull policy",
                "default": "IfNotPresent",
                "enum": ["Always", "IfNotPresent", "Never"],
            },
        },
    }


REPLICAS = {
    "type": "integer",
    "description": "Number of operator pod replicas",
    "default": 2,
    "minimum": 1,
}


def _resource_quantity_field(description, default):
    return {
        "description": description,
        "default": default,
        "allOf": [{"$ref": "#/definitions/resourceQuantity"}],
    }


RESOURCES = {
    "type": "object",
    "description": "Resource requests and limits for the operator container",
    "additionalProperties": False,
    "properties": {
        "limits": {
            "type": "object",
            "description": "Resource limits for the operator container",
            "additionalProperties": False,
            "properties": {
                "cpu": _resource_quantity_field("CPU resource limit", "500m"),
                "memory": _resource_quantity_field("Memory resource limit", "128Mi"),
            },
        },
        "requests": {
            "type": "object",
            "description": "Resource requests for the operator container",
            "additionalProperties": False,
            "properties": {
                "cpu": _resource_quantity_field("CPU resource request", "10m"),
                "memory": _resource_quantity_field("Memory resource request", "64Mi"),
            },
        },
    },
}

RBAC = {
    "type": "object",
    "description": "RBAC configuration for the operator",
    "additionalProperties": False,
    "properties": {
        "namespaceScoped": {
            "type": "boolean",
            "description": "Deploy with namespace-scoped Role/RoleBinding instead of ClusterRole/ClusterRoleBinding. Restricts the operator to its release namespace. Requires webhook.enabled=false.",
            "default": False,
        }
    },
}

LEADER_ELECTION = {
    "type": "object",
    "description": "Leader election configuration for high availability",
    "additionalProperties": False,
    "properties": {
        "enabled": {
            "type": "boolean",
            "description": "Enable leader election for high availability",
            "default": True,
        }
    },
}

CONTROLLER = {
    "type": "object",
    "description": "Controller-runtime reconcile-loop tuning for the operator",
    "additionalProperties": False,
    "properties": {
        "maxConcurrentReconciles": {
            "type": "integer",
            "description": "Maximum number of CRs that reconcile concurrently (controller-runtime MaxConcurrentReconciles), rendered as --max-concurrent-reconciles. Applied by controllers that opt in; the c5c3 operator accepts the flag but does not yet consume it.",
            "default": 2,
            "minimum": 1,
        }
    },
}


def webhook_property(enabled_description):
    return {
        "type": "object",
        "description": "Admission webhook configuration",
        "additionalProperties": False,
        "properties": {
            "enabled": {
                "type": "boolean",
                "description": enabled_description,
                "default": True,
            }
        },
    }


METRICS = {
    "type": "object",
    "description": "Metrics endpoint configuration",
    "additionalProperties": False,
    "properties": {
        "port": {
            "type": "integer",
            "description": "Port for the Prometheus metrics endpoint",
            "default": 8080,
            "minimum": 1,
            "maximum": 65535,
        }
    },
}

LOGGING = {
    "type": "object",
    "description": "Operator zap logger configuration. Production defaults (development=false): JSON encoder, info level, error-level stack traces.",
    "additionalProperties": False,
    "properties": {
        "development": {
            "type": "boolean",
            "description": "Enable development logging mode (--zap-devel): console encoder, debug verbosity, warn-level stack traces. Leave false for production.",
            "default": False,
        },
        "level": {
            "type": "string",
            "description": "Zap log level (--zap-log-level): debug, info, error, panic, or a positive integer for custom verbosity. Empty uses the mode default.",
            "default": "",
            "pattern": r"^(|debug|info|error|panic|[1-9][0-9]*)$",
        },
        "encoder": {
            "type": "string",
            "description": "Zap log encoder (--zap-encoder): json or console. Empty uses the mode default.",
            "default": "",
            "enum": ["", "json", "console"],
        },
    },
}

MONITORING = {
    "type": "object",
    "description": "Observability integration configuration",
    "additionalProperties": False,
    "properties": {
        "serviceMonitor": {
            "type": "object",
            "description": "Prometheus Operator ServiceMonitor settings. Requires the monitoring.coreos.com CRDs in-cluster when enabled.",
            "additionalProperties": False,
            "properties": {
                "enabled": {
                    "type": "boolean",
                    "description": "Render a ServiceMonitor targeting the operator metrics port",
                    "default": False,
                },
                "interval": {
                    "type": "string",
                    "description": "Scrape interval (Go duration: e.g., 15s, 30s, 1m, or 0 to use the global default)",
                    "default": "30s",
                    "pattern": r"^(0|([0-9]+(ns|us|µs|ms|s|m|h))+)$",
                },
            },
        }
    },
}

SERVICE_ACCOUNT = {
    "type": "object",
    "description": "Service account configuration",
    "additionalProperties": False,
    "properties": {
        "create": {
            "type": "boolean",
            "description": "Create a service account for the operator",
            "default": True,
        },
        "name": {
            "type": "string",
            "description": "Name of the service account; if empty and create is true, a name is generated from the fullname template",
            "default": "",
        },
    },
}

NAME_OVERRIDE = {
    "type": "string",
    "description": "Override the chart name used in resource names",
    "default": "",
}

FULLNAME_OVERRIDE = {
    "type": "string",
    "description": "Override the full resource name prefix, skipping release-name prefixing",
    "default": "",
}

OPERATOR_LIBRARY = {
    "type": "object",
    "description": "Reserved values namespace for the operator-library library subchart. The library carries no configurable values; Helm injects this (empty) key during values coalescing, so additionalProperties:false at the root must permit it.",
}

# --- keystone-only property + constraints -----------------------------------

NETWORK_POLICY = {
    "type": "object",
    "description": "NetworkPolicy for operator pod egress/ingress restriction. Opt-in via enabled=true.",
    "additionalProperties": False,
    "properties": {
        "enabled": {
            "type": "boolean",
            "description": "Render a NetworkPolicy that default-denies operator pod traffic except for the explicit rules below",
            "default": False,
        },
        "kubeApiServer": {
            "type": "object",
            "description": "Kubernetes API server reachability. Required (non-empty) when enabled=true — fail-closed",
            "additionalProperties": False,
            "properties": {
                "cidrs": {
                    "type": "array",
                    "description": "List of kube-apiserver CIDR blocks the operator may reach for egress and receive webhook calls from",
                    "items": {"allOf": [{"$ref": "#/definitions/cidr"}]},
                    "default": [],
                },
                "ports": {
                    "type": "array",
                    "description": "List of kube-apiserver TCP ports the operator may reach (e.g., 443, 6443)",
                    "items": {"type": "integer", "minimum": 1, "maximum": 65535},
                    "default": [],
                },
            },
        },
        "dns": {
            "type": "object",
            "description": "DNS egress configuration for kube-dns resolution",
            "additionalProperties": False,
            "properties": {
                "enabled": {
                    "type": "boolean",
                    "description": "Permit UDP+TCP/53 egress to the DNS peer below",
                    "default": True,
                },
                "namespaceSelector": {
                    "description": "namespaceSelector matchLabels for the DNS peer (defaults to kube-system). minProperties:1 prevents an empty map from silently broadening the DNS peer to match all namespaces.",
                    "allOf": [{"$ref": "#/definitions/stringMap"}],
                    "minProperties": 1,
                    "default": {"kubernetes.io/metadata.name": "kube-system"},
                },
                "podSelector": {
                    "description": "podSelector matchLabels for the DNS peer (defaults to k8s-app=kube-dns). minProperties:1 prevents an empty map from silently broadening the DNS peer to match all pods.",
                    "allOf": [{"$ref": "#/definitions/stringMap"}],
                    "minProperties": 1,
                    "default": {"k8s-app": "kube-dns"},
                },
            },
        },
        "allowMetricsFrom": {
            "type": "array",
            "description": "List of NetworkPolicyPeer objects permitted to scrape the metrics port. Empty disables metrics ingress",
            "items": {"type": "object"},
            "default": [],
        },
        "webhookClients": {
            "type": "object",
            "description": "Webhook admission ingress source override. Only consulted when webhook.enabled=true",
            "additionalProperties": False,
            "properties": {
                "cidrs": {
                    "type": "array",
                    "description": "CIDR blocks permitted to call the webhook on 9443. When empty, falls back to kubeApiServer.cidrs",
                    "items": {"allOf": [{"$ref": "#/definitions/cidr"}]},
                    "default": [],
                }
            },
        },
    },
}

RBAC_WEBHOOK_RULE = {
    "if": {
        "properties": {
            "rbac": {
                "properties": {"namespaceScoped": {"const": True}},
                "required": ["namespaceScoped"],
            }
        },
        "required": ["rbac"],
    },
    "then": {"properties": {"webhook": {"properties": {"enabled": {"const": False}}}}},
}

NETWORK_POLICY_RULE = {
    "if": {
        "properties": {
            "networkPolicy": {
                "properties": {"enabled": {"const": True}},
                "required": ["enabled"],
            }
        },
        "required": ["networkPolicy"],
    },
    "then": {
        "properties": {
            "networkPolicy": {
                "properties": {
                    "kubeApiServer": {
                        "properties": {
                            "cidrs": {"minItems": 1},
                            "ports": {"minItems": 1},
                        },
                        "required": ["cidrs", "ports"],
                    }
                },
                "required": ["kubeApiServer"],
            }
        }
    },
}

# --- Per-chart configuration ------------------------------------------------

# The webhook.enabled description names the chart's CR kind, which is not
# discoverable from the chart directory - every operator chart must have an
# entry here (discover_charts fails loudly on a missing one, so adding a new
# operator surfaces this file in the touch list).
WEBHOOK_ENABLED_DESCRIPTIONS = {
    "keystone-operator": "Enable admission webhooks for Keystone CR validation and defaulting",
    "c5c3-operator": "Enable admission webhooks for ControlPlane CR validation and defaulting",
    "horizon-operator": "Enable admission webhooks for Horizon CR validation and defaulting",
}


def discover_charts():
    """Discover the operator charts from the repository layout.

    Every chart under operators/<op>/helm/<op>-operator/ participates. The
    image repository default is read from the chart's values.yaml, and the
    NetworkPolicy schema pieces are included exactly when the chart ships a
    templates/networkpolicy.yaml. A new operator following the directory
    convention is picked up without editing a hardcoded chart list.
    """
    charts = []
    for chart_dir in sorted(REPO_ROOT.glob("operators/*/helm/*-operator")):
        name = chart_dir.name
        if name not in WEBHOOK_ENABLED_DESCRIPTIONS:
            sys.exit(
                f"error: chart {name} has no WEBHOOK_ENABLED_DESCRIPTIONS entry "
                f"in {Path(__file__).name}; add one for the new operator"
            )
        repository = None
        # values.yaml is flat enough that a targeted scan beats a YAML
        # dependency: the first `repository:` key under `image:` is the one.
        in_image = False
        for line in (chart_dir / "values.yaml").read_text().splitlines():
            if line.startswith("image:"):
                in_image = True
                continue
            if in_image and not line.startswith((" ", "\t")):
                in_image = False
            if in_image and line.strip().startswith("repository:"):
                repository = line.split(":", 1)[1].strip()
                break
        if repository is None:
            sys.exit(f"error: could not find image.repository in {chart_dir}/values.yaml")
        charts.append(
            {
                "path": str((chart_dir / "values.schema.json").relative_to(REPO_ROOT)),
                "name": name,
                "image_repository_default": repository,
                "webhook_enabled_description": WEBHOOK_ENABLED_DESCRIPTIONS[name],
                "network_policy": (chart_dir / "templates" / "networkpolicy.yaml").exists(),
            }
        )
    return charts


CHARTS = discover_charts()


def build_schema(chart):
    """Assemble one chart's full values schema from the shared blocks."""
    definitions = {"resourceQuantity": RESOURCE_QUANTITY}

    properties = {
        "image": image_property(chart["image_repository_default"]),
        "replicas": REPLICAS,
        "resources": RESOURCES,
        "rbac": RBAC,
        "leaderElection": LEADER_ELECTION,
        "controller": CONTROLLER,
        "webhook": webhook_property(chart["webhook_enabled_description"]),
        "metrics": METRICS,
        "logging": LOGGING,
        "monitoring": MONITORING,
        "serviceAccount": SERVICE_ACCOUNT,
    }
    all_of = [RBAC_WEBHOOK_RULE]

    if chart["network_policy"]:
        definitions["cidr"] = CIDR
        definitions["stringMap"] = STRING_MAP
        properties["networkPolicy"] = NETWORK_POLICY
        all_of.append(NETWORK_POLICY_RULE)

    properties["nameOverride"] = NAME_OVERRIDE
    properties["fullnameOverride"] = FULLNAME_OVERRIDE
    properties["operator-library"] = OPERATOR_LIBRARY

    return {
        "$schema": "http://json-schema.org/draft-07/schema#",
        "title": f"{chart['name']} Helm Values",
        "description": f"Schema for the {chart['name']} Helm chart values",
        "type": "object",
        "additionalProperties": False,
        "definitions": definitions,
        "properties": properties,
        "allOf": all_of,
    }


def render(chart):
    """Return the exact bytes that should live in the chart's schema file."""
    return json.dumps(build_schema(chart), indent=2, ensure_ascii=False) + "\n"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--check",
        action="store_true",
        help="exit non-zero (without writing) if any committed file is stale",
    )
    args = parser.parse_args()

    drift = False
    for chart in CHARTS:
        path = REPO_ROOT / chart["path"]
        want = render(chart)
        if args.check:
            have = path.read_text(encoding="utf-8") if path.exists() else ""
            if have != want:
                drift = True
                print(f"DRIFT: {chart['path']} is out of sync with the generator")
        else:
            path.write_text(want, encoding="utf-8")
            print(f"wrote {chart['path']}")

    if args.check and drift:
        print("Helm values schema drift detected. Run 'make gen-helm-schema'.")
        return 1
    if args.check:
        print("Helm values schema check passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
