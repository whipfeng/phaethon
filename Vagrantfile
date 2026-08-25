Vagrant.configure("2") do |config|
  config.vm.box = "generic/alpine319"
  config.vm.hostname = "docker-vm"
  config.vm.network "forwarded_port", guest: 22, host: 2222
  config.vm.network "forwarded_port", guest: 2375, host: 2375

  # 如需经过宿主机代理下载，可在此设置 http_proxy/https_proxy
  # export http_proxy="http://127.0.0.1:7893"
  # export https_proxy="http://127.0.0.1:7893"
  config.vm.provision "shell", inline: <<-SHELL
    echo "nameserver 8.8.8.8" > /etc/resolv.conf
    apk add docker docker-compose
    rc-update add docker boot
    service docker start
    echo 'DOCKER_OPTS="-H tcp://0.0.0.0:2375 -H unix:///var/run/docker.sock"' >> /etc/conf.d/docker
    service docker restart
  SHELL

  config.vm.provider "virtualbox" do |vb|
    vb.memory = "1024"
    vb.cpus = 1
    vb.name = "docker-vm"
  end
end
