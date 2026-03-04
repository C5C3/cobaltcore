# Pattern: Operator manager entrypoint structure

**Component**: operators/*/main.go
**Category**: service-structure
**Applies-When**: Creating a new operator module (e.g., operators/glance/, operators/nova/)

## Description

Every operator main.go follows a fixed 6-step structure: (1) scheme registration in init() with clientgoscheme + kubebuilder scaffold marker, (2) flag parsing for metrics-bind-address, health-probe-bind-address, leader-elect, and zap options, (3) ctrl.NewManager with metricsserver.Options (not deprecated MetricsBindAddress string), (4) kubebuilder scaffold:builder marker for controller registration, (5) AddHealthzCheck + AddReadyzCheck with healthz.Ping, (6) mgr.Start(ctrl.SetupSignalHandler()). All errors cause setupLog.Error + os.Exit(1). LeaderElectionID follows the convention '<operator-name>.openstack.c5c3.io'.

## Examples

### `operators/keystone/main.go:36`

```go
func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager, ensuring only one active controller manager.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "keystone.openstack.c5c3.io",
	})
```

### `operators/c5c3/main.go:36`

```go
func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager, ensuring only one active controller manager.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "c5c3.openstack.c5c3.io",
	})
```

