#!/bin/bash

nodenames=(
"alice"
"bob"
"charlie"
"derek"
"emily"
"frank"
"gina"
)

# generate an invoice, takes node name and amt
geninvoice() {
  docker exec -it $1 lncli addinvoice --amt $2 | grep -Po "pay_req\": \"\K[^\"]+"
}
# pay an invoice, takes nodename and invoice
payinvoice() {
  docker exec -it $1 lncli payinvoice $2
}
# simulate a payment, arg is a string of two node IDs
simpayment() {
  sender=${1%[0-9]}
  receiver=${1#[0-9]}

  echo "Trying to make ${nodenames[$sender]} pay ${nodenames[$receiver]}"
  invoice=$(geninvoice ${nodenames[$receiver]} 20000)
  payinvoice ${nodenames[$sender]} $invoice
}


if [[ -z $1 ]]; then
  i=0; for n in ${nodenames[@]}; do
    echo "$n: $i"
    let i=i+1
  done

  echo "To transact, pick two numbers and concatenate them."
  echo "example: for alice to pay bob, enter 01"
  read -p "Pick nodes to transact: " t
  simpayment $t
else
  simpayment $1
fi
