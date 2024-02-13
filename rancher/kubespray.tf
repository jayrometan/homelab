resource "null_resource" "kubespray" {

  provisioner "remote-exec" {
    inline = [
      "mkdir -p /tmp/kubespray",
      "rm -rf /tmp/kubespray"
    ]
    connection {
      type     = "ssh"
      user     = "root"
      password = "951623"
      host     = "192.168.1.25"
    }
  }

  provisioner "file" {
    source      = "kubespray/inventory/jay"
    destination = "/tmp/kubespray"
  
    connection {
      type     = "ssh"
      user     = "root"
      password = "951623"
      host     = "192.168.1.25"
    }
  }

  provisioner "remote-exec" {
    inline = [
      "podman pull quay.io/kubespray/kubespray:v2.24.0",
      "podman run --rm -it --mount type=bind,source=/tmp/kubespray/,dst=/kubespray/inventory,z --mount type=bind,source=/root/.ssh/id_rsa,dst=/root/.ssh/id_rsa,z quay.io/kubespray/kubespray:v2.24.0 bash -c 'ansible-playbook -i /kubespray/inventory/inventory.ini --private-key /root/.ssh/id_rsa cluster.yml'"
    ]
    connection {
      type     = "ssh"
      user     = "root"
      password = "951623"
      host     = "192.168.1.25"
    }
  }

  triggers = {
    configuration_files = "${join(",", sort([for f in fileset("kubespray/inventory/jay", "kubespray/inventory/jay/*"): filemd5(f)]))}"
  }

}
