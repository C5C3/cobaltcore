# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The text of nova's rootwrap.conf for an image whose programs live in
# /nix/store; images/nova-compute/image.nix writes it to
# /etc/nova/rootwrap.conf. filters_path names the venv's filters, and
# exec_dirs starts with /var/lib/openstack/bin and continues with store bin
# directories instead of the FHS ones. The other keys are those of nova's
# etc/nova/rootwrap.conf.
#
#   filtersPath  the directory of the *.filters files
#   execDirs     store bin directories rootwrap searches after
#                /var/lib/openstack/bin; must not be empty
{ filtersPath, execDirs }:
if execDirs == [ ] then
  throw "rootwrap-conf: execDirs is empty; rootwrap resolves every filtered command through exec_dirs, so name at least the store bin directory of the tools the filters allow"
else
  ''
    # Written by nix/lib/rootwrap-conf.nix.
    [DEFAULT]
    filters_path=${filtersPath}
    exec_dirs=/var/lib/openstack/bin,${builtins.concatStringsSep "," execDirs}
    use_syslog=False
    syslog_log_facility=syslog
    syslog_log_level=ERROR
    daemon_timeout=600
    rlimit_nofile=1024
  ''
