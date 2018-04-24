#!/bin/bash
export NETWORK="simnet"

topologies=(
"twonodes"
"threenodes"
"sevennodes"
)

nodenames=(
"alice"
"bob"
"charlie"
"derek"
"emily"
"frank"
"gina"
)

# generate blocks, arg is number of blocks
genblocks() {
  docker-compose run btcctl generate $1 > /dev/null
}
# generate a new address, takes node name as arg
newaddress() {
  docker exec -t $1 lncli newaddress np2wkh | grep -Po "address\": \"\K[^\"]+"
}
# send coins, node name, address amt as args
sendcoins() {
  docker exec -t $1 lncli sendcoins --addr $2 --amt $3
}
# get the IP of a node, takes node name as arg
getip() {
  docker inspect $1 | grep IPAddress | tail -n 1 | grep -Po "IPAddress\": \"\K[^\"]+"
}
# get pubkey for a node, takes node name as arg
getpubkey() {
  docker exec -t $1 lncli getinfo | grep pubkey | tail -n 1 | grep -Po "pubkey\": \"\K[^\"]+"
}
# connect to a node, takes node name, remote pubkey, and remote IP
connectnode() {
  docker exec -t $1 lncli connect $2@$3 > /dev/null
}
# open a channel, takes node name, remote pubkey and amt
openchannel() {
  docker exec -t $1 lncli openchannel --node_key=$2 --local_amt=$3
}
color() {
  echo -e "\e[36m$1\e[0m\n"
}


# cleanup old stuff
cleanup() {
  color "Stopping all previous containers"
  docker stop -t 0 btcd
  for node in ${nodenames[@]}; do
    docker stop -t 0 $node
  done
  color "Removing all containers and volumes, respond with Y at the next two prompts"
  docker container prune
  docker volume prune
}

twonodes() {
  cleanup
  color "Creating this configuration: [${nodenames[0]}]->[${nodenames[1]}]"
  i=0; while [ $i -lt 2 ]; do
    docker-compose run -d --name ${nodenames[$i]} lnd_btc
    let i=i+1
  done
  color "Starting btcd" && sleep 5
  address=$(newaddress ${nodenames[0]})

  # bring up btcd and give alice some coins
  MINING_ADDRESS=$address docker-compose up -d btcd
  color "Generating 400 blocks" && sleep 5
  genblocks 400 && sleep 5

  firstip=$(getip ${nodenames[0]})
  firstpubkey=$(getpubkey ${nodenames[0]})
  secondip=$(getip ${nodenames[1]})
  secondpubkey=$(getpubkey ${nodenames[1]})

  color "connecting ${nodenames[0]} and ${nodenames[1]}"
  connectnode ${nodenames[0]} $secondpubkey $secondip
  openchannel ${nodenames[0]} $secondpubkey 1000000

  genblocks 10
  color "Completed, ${nodenames[0]} opened a channel to ${nodenames[1]} for 1,000,000 satoshis and the channel is now active."
}

threenodes() {
  # initialize two node setup
  twonodes
  color "Creating this configuration: [${nodenames[0]}]->[${nodenames[1]}]->[${nodenames[2]}]"
  # get a new address for second node
  address=$(newaddress ${nodenames[1]})
  # send coins from first to second node
  color "Sending ${nodenames[1]} some coins"
  sendcoins ${nodenames[0]} $address 10000000
  # mine the transaction
  genblocks 6
  docker-compose run -d --name ${nodenames[2]} lnd_btc
  color "sleeping 5 to let container start" && sleep 5

  thirdip=$(getip ${nodenames[2]})
  thirdpubkey=$(getpubkey ${nodenames[2]})

  # open a channel from second node to third node
  color "connecting ${nodenames[1]} and ${nodenames[2]}"
  connectnode ${nodenames[1]} $thirdpubkey $thirdip
  openchannel ${nodenames[1]} $thirdpubkey 1000000

  # mine 6 blocks to activate channel
  genblocks 10
  color "Completed, ${nodenames[1]} opened a channel to ${nodenames[2]} for 1,000,000 satoshis and the channel is now active."
}

sevennodes() {
  threenodes
  color '''Creating this configuration:

[${nodenames[0]}]->[${nodenames[1]}]->[${nodenames[2]}]
   V ^--._______-^  V
[${nodenames[3]}]-`[${nodenames[4]}]  [${nodenames[5]}]
           ^
         [${nodenames[6]}]

(see: https://i.imgur.com/PwRux76.png)
'''
  i=3; while [ $i -lt 7 ]; do
    docker-compose run -d --name ${nodenames[$i]} lnd_btc
    let i=i+1
  done

  color "sleeping 5 to let containers start" && sleep 5
  color "sending ${nodenames[0]}'s funds to nodes that need them"
  # give all the nodes that need funds funds
  for i in {2,3,4,6}; do
    address=$(newaddress ${nodenames[$i]})
    sendcoins ${nodenames[0]} $address 10000000
    genblocks 1
  done
  color "sleeping 5 to let blocks propagate" && sleep 5

  # get the rest of the necessary ips and pubkeys
  fourthip=$(getip ${nodenames[3]})
  fourthpubkey=$(getpubkey ${nodenames[3]})
  fifthip=$(getip ${nodenames[4]})
  fifthpubkey=$(getpubkey ${nodenames[4]})
  sixthip=$(getip ${nodenames[5]})
  sixthpubkey=$(getpubkey ${nodenames[5]})

  # connect alice and derek
  color "connecting ${nodenames[0]} and ${nodenames[3]}"
  connectnode ${nodenames[0]} $fourthpubkey $fourthip
  openchannel ${nodenames[0]} $fourthpubkey 1000000
  genblocks 6
  # connect charlie and frank
  color "connecting ${nodenames[2]} and ${nodenames[5]}"
  connectnode ${nodenames[2]} $sixthpubkey $sixthip
  openchannel ${nodenames[2]} $sixthpubkey 1000000
  genblocks 6
  # connect derek and charlie
  color "connecting ${nodenames[3]} and ${nodenames[2]}"
  connectnode ${nodenames[3]} $thirdpubkey $thirdip
  openchannel ${nodenames[3]} $thirdpubkey 1000000
  genblocks 6
  # connect emily and alice
  color "connecting ${nodenames[4]} and ${nodenames[0]}"
  connectnode ${nodenames[4]} $firstpubkey $firstip
  openchannel ${nodenames[4]} $firstpubkey 1000000
  genblocks 6
  # connect gina and emily
  color "connecting ${nodenames[6]} and ${nodenames[4]}"
  connectnode ${nodenames[6]} $fifthpubkey $fifthip
  openchannel ${nodenames[6]} $fifthpubkey 1000000
  genblocks 10

  color "Completed, configuration is now complex with 9 nodes."
}

if [[ -z $1 ]]; then
  echo -e "--possible topologies-- \n${topologies[@]}"
  read -p "pick a topology: " topology

  $topology
else
  $1
fi
