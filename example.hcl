job "example" {
  datacenters = ["dc1"]
  type        = "service"

  group "group1" {
    count = 1
            
    network {
      port "http" {}
    } 

    task "task1" {
      driver = "pot"
      
      service {
        tags = ["pot-jail", "metrics"]
        name = "pot-example"
        port = "http"
       
         check {
            type     = "tcp"
            name     = "http"
            interval = "5s"
            timeout  = "2s"
          }
      }


      config {
        image = "https://pot-registry.zapto.org/registry/"
        pot = "nginx-only"
        tag = "1.0"
        command = "nginx -g 'daemon off;'"
        port_map = { 
          http = "80"
        }
        network_mode = "host"
        copy = [ "/tmp/test.txt:/root/test.txt", "/tmp/test2.txt:/root/test2.txt" ]
        mount = [ "/tmp/test:/root/test", "/tmp/test2:/root/test2" ]
        mount_read_only = [ "/tmp/test2:/root/test2" ] 
      }
      
      resources {
        cpu = 200
        memory = 128
      }
    }
  }
}
