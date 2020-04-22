/Users/wyang/spark-2.4.4-bin-hadoop2.7/bin/spark-submit \
       --master k8s://http://localhost:8001 --deploy-mode cluster --name spark-pi \
       --class org.apache.spark.examples.SparkPi \
       --conf spark.executor.instances=1 \
       --conf spark.kubernetes.container.image=yunikorn/spark:2.4.4 \
       --conf spark.kubernetes.driver.label.queue="root.sandbox" \
       --conf spark.kubernetes.driver.label.queue="root.sandbox" \
       --conf spark.kubernetes.executor.label.queue="root.sandbox" \
       --conf spark.kubernetes.authenticate.driver.serviceAccountName=spark \
       local:///opt/spark/examples/jars/spark-examples_2.11-2.4.4.jar
