package generation

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"

	"github.com/c-robinson/iplib/v2"
	"github.com/softlayer/softlayer-go/datatypes"
	"github.com/vmware/govmomi/vim25/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift-splat-team/vsphere-capacity-manager-data/pkg/ibmcloud"
	"github.com/openshift-splat-team/vsphere-capacity-manager-data/pkg/vsphere"
	vcmv1 "github.com/openshift-splat-team/vsphere-capacity-manager/pkg/apis/vspherecapacitymanager.splat.io/v1"
	configv1 "github.com/openshift/api/config/v1"
)

type PortGroupSubnet struct {
	// IBM and Port Group vlan id
	VlanId int32
	// Port Group name
	Name string

	// IBM Variables
	PodName        *string
	DatacenterName *string
	Subnets        []datatypes.Network_Subnet

	// vcenter server
	// TODO: we may not be able to do this association
	// TODO: and maybe we don't even need it.
	// TODO: problem: if I have two vcenters in the same datacenter and pod they most likely will have
	// TODO: the same vlans attached
	//Server string
}

type FailureDomainResourceCapacity struct {
	NumCpuCores int16
	TotalMemory int64
	Name        string
}

type VSphereEnvironmentsConfig struct {
	configv1.VSpherePlatformSpec
	PortGroupSubnets               []PortGroupSubnet
	FailureDomainsResourceCapacity []FailureDomainResourceCapacity
}

func parseIBMCredentails(ibmCloudAuthFileName string) (map[string]ibmcloud.SoftlayerCredentials, error) {
	ibmCredentails := make(map[string]ibmcloud.SoftlayerCredentials)

	b, err := os.ReadFile(ibmCloudAuthFileName)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(b, &ibmCredentails)
	if err != nil {
		return nil, err
	}

	return ibmCredentails, nil
}
func parseVSphereCredentails(vcenterAuthFileName string) (map[string]vsphere.VCenterCredential, error) {
	vCenterCredentails := make(map[string]vsphere.VCenterCredential)

	b, err := os.ReadFile(vcenterAuthFileName)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(b, &vCenterCredentails)
	if err != nil {
		return nil, err
	}
	return vCenterCredentails, nil
}

func CreateVSphereEnvironmentsConfig(vCenterAuthFileName, ibmCloudAuthFileName, ipv6SubnetString, portGroupSubString string) (*VSphereEnvironmentsConfig, error) {
	var envs VSphereEnvironmentsConfig

	vmeta := vsphere.NewMetadata()
	imeta := ibmcloud.NewMetadata()

	ibmCredentails, err := parseIBMCredentails(ibmCloudAuthFileName)
	if err != nil {
		return nil, err
	}

	for a, i := range ibmCredentails {
		err := imeta.AddCredentials(a, i.Username, i.ApiToken)
		if err != nil {
			return nil, err
		}
	}

	vcenterCredentials, err := parseVSphereCredentails(vCenterAuthFileName)

	for k, v := range vcenterCredentials {
		_, err := vmeta.AddCredentials(k, v.Username, v.Password)
		if err != nil {
			log.Fatal(err)
		}

		failureDomains, err := vmeta.GetFailureDomainsViaTag(k)
		if failureDomains == nil {
			if err != nil {
				log.Printf("WARNING: No failure domains found for %s, %s", k, err)
			} else {
				log.Printf("WARNING: No failure domains found for %s", k)
			}
			continue
		}

		for _, fd := range *failureDomains {
			cObj, err := vmeta.GetClusterByPath(fd.Server, fd.Topology.ComputeCluster)
			if err != nil {
				return nil, err
			}

			cpu, memory, err := vmeta.GetClusterCapacity(fd.Server, cObj)
			if err != nil {
				return nil, err
			}

			envs.FailureDomainsResourceCapacity = append(envs.FailureDomainsResourceCapacity, FailureDomainResourceCapacity{
				Name:        fd.Name,
				NumCpuCores: cpu,
				TotalMemory: memory,
			})

			pool := vcmv1.Pool{
				TypeMeta: metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{
					Name: fd.Name,
				},
				Spec: vcmv1.PoolSpec{
					VSpherePlatformFailureDomainSpec: fd,
					VCpus:                            int(cpu),
					Memory:                           int(memory / 1024 / 1024 / 1024),
					Storage:                          0,
					Exclude:                          false,
				},
			}

			b, err := json.MarshalIndent(pool, "", "  ")
			if err != nil {
				log.Print(err)
			}

			fmt.Print(string(b))

		}

		envs.FailureDomains = append(envs.FailureDomains, *failureDomains...)

		var dcPaths []string
		datacenters, err := vmeta.GetDatacenters(k)
		if err != nil {
			return nil, err
		}

		for _, dc := range datacenters {
			dcPaths = append(dcPaths, dc.InventoryPath)
		}

		envs.VCenters = append(envs.VCenters, configv1.VSpherePlatformVCenterSpec{
			Server:      k,
			Datacenters: dcPaths,
		})

		portGroups, err := vmeta.GetDistributedPortGroups(k, portGroupSubString)
		if err != nil {
			return nil, err
		}

		portGroupSubnetsMap := make(map[int32]PortGroupSubnet)

		for _, pg := range portGroups {
			portSetting := pg.Config.DefaultPortConfig.(*types.VMwareDVSPortSetting)
			vlanId := portSetting.Vlan.(*types.VmwareDistributedVirtualSwitchVlanIdSpec).VlanId

			portGroupSubnetsMap[vlanId] = PortGroupSubnet{
				Name:   pg.Config.Name,
				VlanId: vlanId,
			}
		}

		url, err := vmeta.GetHostnameUrlVpxd(k)
		if err != nil {
			return nil, err
		}

		if k != *url {
			log.Printf("WARN: vCenter URL does not match %s != %s", k, *url)
		}

		vcIP, err := net.LookupIP(k)
		if err != nil {
			log.Fatal(err)
		}

		var networkVlans *[]datatypes.Network_Vlan
		var vcLocation *ibmcloud.VCenterLocation
		for account, _ := range ibmCredentails {
			vcLocation, err = imeta.FindVCenterPhyDC(account, vcIP)
			if err != nil {
				return nil, err
			}

			if vcLocation.DatacenterName != nil {
				networkVlans, err = imeta.GetVlanSubnets(account, *vcLocation.DatacenterName, *vcLocation.PodName)
				if err != nil {
					return nil, err
				}
				break
			}
		}

		if networkVlans == nil {
			if vcLocation != nil && vcLocation.PodName != nil {
				log.Printf("WARNING: unable to retrieve IBM network subnets in datacenter pod %s vCenter %s using IP address %s", *vcLocation.PodName, k, vcIP[0].String())
			} else {
				log.Printf("WARNING: unable to find physcial location of vCenter %s using IP address %s", k, vcIP[0].String())
			}

			continue
		}

		for _, nv := range *networkVlans {
			vlanNumber := int32(*nv.VlanNumber)
			if pg, ok := portGroupSubnetsMap[vlanNumber]; ok {

				if len(nv.Subnets) > 1 {
					log.Print("WARNING: the length of the vlan subnet is greater then one, using only the first entry")
				}

				subnet := nv.Subnets[0]

				ipAddressesAsString := make([]string, 0, len(subnet.IpAddresses))
				for _, ipAddress := range subnet.IpAddresses {
					ipAddressesAsString = append(ipAddressesAsString, *ipAddress.IpAddress)
				}

				ipv6Subnet := iplib.Net6FromStr(fmt.Sprintf("%s:%d::1/64", ipv6SubnetString, *nv.VlanNumber))

				cidrV6, _ := ipv6Subnet.Mask().Size()

				network := vcmv1.Network{
					ObjectMeta: metav1.ObjectMeta{
						Name: pg.Name,
					},
					Spec: vcmv1.NetworkSpec{
						PortGroupName:      pg.Name,
						VlanId:             string(rune(*nv.VlanNumber)),
						PodName:            subnet.PodName,
						DatacenterName:     subnet.Datacenter.Name,
						Cidr:               subnet.Cidr,
						Gateway:            subnet.Gateway,
						IpAddressCount:     subnet.IpAddressCount,
						Netmask:            subnet.Netmask,
						SubnetType:         subnet.SubnetType,
						MachineNetworkCidr: fmt.Sprintf("%s/%d", *subnet.NetworkIdentifier, *subnet.Cidr),
						IpAddresses:        ipAddressesAsString,
						CidrIPv6:           cidrV6,
						GatewayIPv6:        ipv6Subnet.Enumerate(1, 2)[0].String(),
						IpV6prefix:         ipv6Subnet.String(),
						StartIPv6Address:   ipv6Subnet.Enumerate(1, 4)[0].String(),
					},
				}

				// testing only
				b, err := json.MarshalIndent(network, "", "  ")
				if err != nil {
					log.Print(err)
				}

				fmt.Print(string(b))
			}
		}
	}

	return &envs, nil
}
