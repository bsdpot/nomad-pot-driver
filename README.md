<p align="center">
<img width="350" src="mascot.png" />
</p>

# Pot task Driver

Name: `pot-task-driver`

The Pot task driver provides an interface for using [pot][pot-github-repo] for dynamically running applications inside a FreeBSD Jail.
You can download the external pot-task-driver [here][pot-task-driver].

This version of the driver requires pot 0.15.0 or greater.

## Complete job example

```hcl
job "example" {
  datacenters = ["datacenter"]
  type        = "service"

  group "group1" {
    count = 1

    task "www1" {
      driver = "pot"

      service {
        tags = ["nginx", "www"]
        name = "nginx-example-service"
        port = "http"

         check {
            type     = "tcp"
            name     = "tcp"
            interval = "60s"
            timeout  = "30s"
          }
      }

      config {
        image = "https://potluck.honeyguide.net/nginx-nomad"
        pot = "nginx-nomad-amd64-13_1"
        tag = "1.1.13"
        command = "nginx"
        args = ["-g","'daemon off;'"]

        copy = [
          "/mnt/s3/web/nginx.conf:/usr/local/etc/nginx/nginx.conf",
        ]
        mount = [
          "/mnt/s3/web/www:/mnt"
        ]
        port_map = {
          http = "80"
        }
      }

      resources {
        cpu = 200
        memory = 64

        network {
          mbits = 10
          port "http" {}
        }
      }
    }
  }
}
```

## Task Configuration

```hcl
task "nginx-pot" {
    driver = "pot"

    config {
      image = "https://potluck.honeyguide.net/nginx-nomad"
      pot = "nginx-nomad-amd64-13_1"
      tag = "1.1.13"
      command = "nginx"
      args = ["-g","'daemon off;'"]

      copy = [
        "/mnt/s3/web/nginx.conf:/usr/local/etc/nginx/nginx.conf",
      ]
      mount = [
        "/mnt/s3/web/www:/mnt"
      ]
      port_map = {
        http = "80"
      }
    }
}
```

The pot task driver supports the following parameters:

* `image` - The url for the http registry from where to get the image.

* `pot` - Name of the image in the registry.

* `tag` - Version of the image.

* `commad` - (Optional) Command that is going to be executed once the jail is started.

* `args` - (Optional. Depends on `commad`) Array of arguments to append to the command.

* `network_mode` - (Optional) Defines the network mode of the pot. Default: **"public-bridge"**

  Possible values are:

  **"public-bridge"**  pot creates an internal virtual network with a NAT table where all traffic is going to be sent.

  **"host"** pot bounds the jail directly to a host port.

* `port_map` - (Optional) Sets the port on which the application is listening inside of the jail. If not set, the application will inherit the port configuration from the image.

* `copy` - (Optional) Copies a file from the host machine to the pot jail in the given directory.

* `mount` - (Optional) Mounts a read/write folder from the host machine to the pot jail.

* `mount_read_only` - (Optional) Mounts a read only directory inside the pot jail.

* `attributes` - (Optional) List of colon separated `KEY:VALUE` pairs of pot jail attributes (e.g. `devfs_ruleset:3`), see  `pot help set-attr` for possible values.

## Client Requirements

`pot-task-driver` requires the following:

* 64-bit FreeBSD 12.4-RELEASE or 13.2-RELEASE host .
* The FreeBSD's Nomad binary (available as a package).
* The pot-task-driver binary placed in the [plugin_dir][plugin_dir] directory.
* Installing [pot][pot-github-repo], version 0.15.6 or greater, and following the install [guide][pot-install-guide].
* Webserver from where to serve the images. (simple file server)
* Following lines need to be included in your rc.conf

```
nomad_user="root"
nomad_env="PATH=/usr/local/bin:/usr/local/sbin:/usr/bin:/usr/sbin:/sbin:/bin"
```

[pot-task-driver]: https://github.com/trivago/nomad-pot-driver/releases/download/v0.9.1/nomad-pot-driver
[plugin_dir]: /docs/configuration/index.html#plugin_dir
[pot-github-repo]: https://github.com/pizzamig/pot
[pot-install-guide]: https://github.com/pizzamig/pot/blob/master/share/doc/pot/Installation.md
