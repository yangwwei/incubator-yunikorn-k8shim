i="0"

while [ $i -lt 5 ]
do
  kubectl create -f ds1.yaml
  i=$[$i+1]
done
