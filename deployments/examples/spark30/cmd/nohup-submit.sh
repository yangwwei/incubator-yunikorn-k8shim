i="1"

while [ $i -lt 6 ]
do
  nohup bash run.sh ${i} > job-${i}.log &
  i=$[$i+1]
done
