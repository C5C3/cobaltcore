// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"

	"github.com/onsi/gomega"
	"k8s.io/utils/ptr"
)

func TestUWSGISpecDefault_FillsZeroValues(t *testing.T) {
	g := gomega.NewWithT(t)

	u := &UWSGISpec{}
	u.Default()

	g.Expect(u.Processes).To(gomega.Equal(DefaultUWSGIProcesses))
	g.Expect(u.Threads).To(gomega.Equal(DefaultUWSGIThreads))
	g.Expect(u.HTTPKeepAlive).To(gomega.HaveValue(gomega.BeTrue()))
}

// An explicit HTTPKeepAlive=false must survive Default — the nil-preserving
// pointer is what distinguishes "unset" from an explicit user choice.
func TestUWSGISpecDefault_PreservesExplicitValues(t *testing.T) {
	g := gomega.NewWithT(t)

	u := &UWSGISpec{Processes: 8, Threads: 4, HTTPKeepAlive: ptr.To(false)}
	u.Default()

	g.Expect(u.Processes).To(gomega.Equal(int32(8)))
	g.Expect(u.Threads).To(gomega.Equal(int32(4)))
	g.Expect(u.HTTPKeepAlive).To(gomega.HaveValue(gomega.BeFalse()))
}

// The owning operator's uwsgi block is optional, so callers invoke Default on a
// possibly-nil pointer: an absent block must stay absent rather than panic.
func TestUWSGISpecDefault_NilReceiverIsNoOp(t *testing.T) {
	g := gomega.NewWithT(t)

	var u *UWSGISpec
	g.Expect(u.Default).NotTo(gomega.Panic())
	g.Expect(u).To(gomega.BeNil())
}
