

ip link set toleafkind1_br type bridge vlan_filtering 1


for vf_index in {0..3}; do
    nic="toleafkind1v${vf_index}"
    tap="${nic}_t"
    br="toleafkind1_br"
    mac=$(printf "52:54:00:ab:cd:%02x" "${slot}")

    ip tuntap add dev "${tap}" mode tap
    
    ip link set "${tap}" master "${br}"
    ip link set "${tap}" up

    QEMU_NIC_ARGS+=(
        -device "pcie-root-port,id=rp${slot},slot=${slot}"
        -netdev "tap,id=${tap},ifname=${tap},script=no,downscript=no"
        -device "igb,bus=rp${slot},netdev=${tap},mac=${mac}"
    )


    echo "SUBSYSTEM==\"net\", ACTION==\"add\", ATTR{address}==\"${mac}\", NAME=\"${nic}\"" >> "${UDEV_RULES}"

    echo "ToLeafKind1 VF ${vf_index}: ${br}: ${nic} <-> ${tap} (mac ${mac})"
    slot=$((slot + 1))
done


# VF0 and VF1 are on trunk
bridge vlan add vid 33 dev "toleafkind1v0_t"
bridge vlan add vid 44 dev "toleafkind1v0_t"
bridge vlan add vid 33 dev "toleafkind1v1_t"
bridge vlan add vid 44 dev "toleafkind1v1_t"


# TAP 2 (fake VF for VLAN 33): access port, PVID 33, untagged egress.
bridge vlan del vid 1 dev "toleafkind1v2_t"
bridge vlan add vid 33 dev "toleafkind1v2_t" pvid untagged

# TAP 3 (fake VF for VLAN 44): access port, PVID 44, untagged egress.
bridge vlan del vid 1 dev "toleafkind1v3_t"
bridge vlan add vid 44 dev "toleafkind1v3_t" pvid untagged

# Allow VLANs 33 and 44 on the bridge itself.
bridge vlan add vid 33 dev "toleafkind1_br" self
bridge vlan add vid 44 dev "toleafkind1_br" self
