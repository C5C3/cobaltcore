# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The rados and rbd extension modules for Python 3.12, built from the ceph
# source nixpkgs ships against its prebuilt librados and librbd. nixpkgs'
# ceph puts its own bindings into ceph.out, whose closure carries the
# daemons as well, and builds them for the Python nixpkgs picks for ceph;
# rebuilding ceph for another Python compiles all of it. These two Cython
# modules need only the headers in ceph.dev and the libraries in ceph.lib,
# and they carry an RPATH into ceph.lib.
#
# $out/lib/python3.12/site-packages holds rados.cpython-312-*.so and
# rbd.cpython-312-*.so and nothing else; nix/lib/service-image.nix puts the
# directory on the venv's path with a .pth file.
{
  ceph,
  python3Packages,
}:
python3Packages.buildPythonPackage {
  pname = "ceph-bindings";
  inherit (ceph) version src;
  pyproject = false;

  # The source is ceph's whole patched tree, 1.7 GiB unpacked; the build
  # reads the two binding directories and src/include, and only those are
  # copied.
  unpackPhase = ''
    runHook preUnpack
    mkdir -p source/src/pybind
    cp -r --no-preserve=mode "$src/src/include" source/src/
    cp -r --no-preserve=mode "$src/src/pybind/rados" "$src/src/pybind/rbd" source/src/pybind/
    sourceRoot=source
    runHook postUnpack
  '';

  # Python 3.12 has no distutils, which both setup.py files import; rbd's
  # also imports packaging.
  nativeBuildInputs = [
    python3Packages.cython
    python3Packages.distutils
    python3Packages.packaging
    python3Packages.setuptools
  ];

  env = {
    CFLAGS = "-I${ceph.dev}/include";
    LDFLAGS = "-L${ceph.lib}/lib";
  };

  configurePhase = ''
    runHook preConfigure
    for header in rados/librados.h rbd/librbd.h; do
      if [ ! -f "${ceph.dev}/include/$header" ]; then
        echo "ceph-bindings: ${ceph.dev}/include/$header is missing" >&2
        exit 1
      fi
    done
    runHook postConfigure
  '';

  # rbd.pyx cimports rados, so rados is built first. c_rados.pxd takes
  # MAX_ERRNO from the source tree's err.h, which ceph.dev does not install;
  # ceph's cmake/modules/Distutils.cmake passes the same -iquote.
  buildPhase = ''
    runHook preBuild
    export CPPFLAGS="-iquote $PWD/src/include"
    (cd src/pybind/rados && python setup.py build_ext --inplace)
    (cd src/pybind/rbd && python setup.py build_ext --inplace)
    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall
    install -D -m 0555 -t "$out/${python3Packages.python.sitePackages}" \
      src/pybind/rados/rados.cpython-312-*.so \
      src/pybind/rbd/rbd.cpython-312-*.so
    runHook postInstall
  '';

  pythonImportsCheck = [
    "rados"
    "rbd"
  ];
}
