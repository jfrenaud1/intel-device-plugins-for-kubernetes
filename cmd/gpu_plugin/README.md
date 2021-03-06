# Build and test Intel GPU device plugin for Kubernetes

### Get source code:
```
$ mkdir -p $GOPATH/src/github.com/intel/
$ cd $GOPATH/src/github.com/intel/
$ git clone https://github.com/intel/intel-device-plugins-for-kubernetes.git
```

### Build GPU device plugin:
```
$ cd $GOPATH/src/github.com/intel/intel-device-plugins-for-kubernetes
$ make gpu_plugin
```

### Verify kubelet socket exists in /var/lib/kubelet/device-plugins/ directory:
```
$ ls /var/lib/kubelet/device-plugins/kubelet.sock
/var/lib/kubelet/device-plugins/kubelet.sock
```

### Run GPU device plugin as administrator:
```
$ sudo $GOPATH/src/github.com/intel/intel-device-plugins-for-kubernetes/cmd/gpu_plugin/gpu_plugin
device-plugin start server at: /var/lib/kubelet/device-plugins/gpu.intel.com-i915.sock
device-plugin registered
```

### Verify GPU device plugin is registered on master:
```
$ kubectl describe node <node name> | grep gpu.intel.com
 gpu.intel.com/i915:  1
 gpu.intel.com/i915:  1
```

### Test GPU device plugin:

1. Build a Docker image with beignet unit tests:
   ```
   $ cd demo
   $ ./build-image.sh ubuntu-demo-opencl
   ```

      This command produces a Docker image named `ubuntu-demo-opencl`.

2. Create a pod running unit tests off the local Docker image:
   ```
   $ kubectl apply -f demo/intelgpu_job.yaml
   ```

3. Review the pod's logs:
   ```
   $ kubectl logs intelgpu-demo-job-xxxx
   ```
