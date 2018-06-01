#!/bin/bash
asciidoctor -v || gem install asciidoctor 

echo "Usage ./run.sh OR ./run.sh path/to/a_guide.adoc [guide.html] [+1 (header-offset)] [http://guide-host:port]"
# mkdir -p html
DIR=${0%%/run.sh}

function render {
   ADOC=${1-../exercise/lab_01.adoc}
   shift
   HTML=${ADOC%.*}.html
   HTML=html/${HTML##*/}
   HTML=${1-$HTML}
   shift
   OFFSET=${1-+1}
   shift
   BASE_URL=${1-http://guides.neo4j.com/intro} 
   shift
   echo rendering $ADOC to $HTML   
echo asciidoctor $ADOC -T $DIR/templates -a allow-uri-read -a experimental -a guides=$BASE_URL -a current=$BASE_URL -a img=$BASE_URL/img -a leveloffset=${OFFSET} -a env-guide= -a guide= -o ${HTML} "$@"
   asciidoctor $ADOC -T $DIR/templates -a allow-uri-read -a guides=$BASE_URL -a icons=font -a current=$BASE_URL -a img=$BASE_URL/img -a leveloffset=${OFFSET} -a env-guide= -a guide= -o ${HTML} "$@"
}

if [ $# == 0 ]; then
   render ../exercise/lab_01.adoc
   render ../exercise/lab_02.adoc
   render ../exercise/lab_03.adoc
   render ../04_create_data_interactive.adoc html/create.html
   render ../05_cypher_deep_dive_starter.adoc html/starter.html
   render ../05_cypher_deep_dive_expert.adoc html/expert.html
   render ../05_cypher_deep_dive_advanced.adoc html/advanced.html
else
   render "$@"
fi

# s3cmd put -P --recursive html/* s3://guides.neo4j.com/intro/
