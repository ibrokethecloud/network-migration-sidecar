## network-device-migaration sidecar

Simple sidecar based on [kubevirt hook example.](kubevirt.io/kubevirt/cmd/example-hook-sidecar)

The sidecar when added looks up the network interfaces in the pod by mac addresses, and updates 
the domain spec if interface name has changed.

This is needed for migration to ordinal interface naming schema to hash based interface naming schema
in kubevirt v1.x