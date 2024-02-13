terraform {
  required_providers {
    proxmox = {
      source = "bpg/proxmox"
      version = "0.46.3"
    }
  }
}

provider "proxmox" {
  endpoint  = "https://192.168.1.24:8006/api2/json"
  api_token = "terraform@pve!terraform-token=57ce3413-1505-4c72-8fbe-0f4485568356"
  # username = "root@pam"
  # password = "951623"
  insecure  = true
  ssh {
    agent    = true
    username = "root"
  }
}

resource "proxmox_virtual_environment_vm" "rocky9_vms" {
  name      = "rocky9-${count.index + 10}"
  count = 1
  node_name = "proxmox"
  on_boot = true
  vm_id = "102"

  agent {
    enabled = true
  }

  initialization {
    user_account {
      # do not use this in production, configure your own ssh key instead!
      username = "jayrome"
      password = "951623"
    }
  }

  cpu {
    sockets = 1
    cores   = 2
  }
  memory {
    dedicated = 2 * 1024
  }

  clone {
    vm_id = 100
  }

  network_device {
    bridge = "vmbr0"
    model = "virtio"
  }

}
