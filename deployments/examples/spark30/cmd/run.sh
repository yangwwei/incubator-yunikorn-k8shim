#!/bin/bash

JOB_ID=${1}
SPARK_HOME=/Users/wyang/spark-3.0.0-preview2-bin-hadoop2.7
K8S_ENDPOINT=http://localhost:8001
SPARK_EXECUTOR_NUM=2
SPARK_DOCKER_IMAGE=yunikorn/spark:2.4.4
SPARK_EXAMPLE_JAR=local:///opt/spark/examples/jars/spark-examples_2.11-2.4.4.jar

# spark submit command
${SPARK_HOME}/bin/spark-submit \
  --master k8s://${K8S_ENDPOINT} --deploy-mode cluster --name spark-pi \
  --class org.apache.spark.examples.SparkPi \
  --conf spark.executor.instances=${SPARK_EXECUTOR_NUM} \
  --conf spark.kubernetes.container.image=${SPARK_DOCKER_IMAGE} \
  --conf spark.kubernetes.driver.podTemplateFile=../driver.yaml \
  --conf spark.kubernetes.executor.podTemplateFile=/Users/wyang/workspace/apache/yunikorn/incubator-yunikorn-k8shim/deployments/examples/spark30/executor.yaml \
  --conf spark.kubernetes.driver.label.queue="root.default" \
  --conf spark.kubernetes.authenticate.driver.serviceAccountName=spark \
  ${SPARK_EXAMPLE_JAR} 100
