# How to write a Java gRPC client for the Lightning Network Daemon

This section enumerates what you need to do to write a client that communicates
with lnd in Java. We'll be using Maven as our build tool.

### Prerequisites
 - Maven
 - running lnd
 - running btcd

### Setup and Installation
#### Project Structure
```
.
├── pom.xml
└── src
    ├── main
       ├── java
       │   └── Main.java
       ├── proto
          ├── google
          │   └── api
          │       ├── annotations.proto
          │       └── http.proto
          └── lnrpc
              └── rpc.proto

```
Note the ***proto*** folder, where all the proto files are kept.

 - [rpc.proto](https://github.com/lightningnetwork/lnd/blob/master/lnrpc/rpc.proto)
 - [annotations.proto](https://github.com/grpc-ecosystem/grpc-gateway/blob/master/third_party/googleapis/google/api/annotations.proto)
 - [http.proto](https://github.com/grpc-ecosystem/grpc-gateway/blob/master/third_party/googleapis/google/api/http.proto)

#### pom.xml
```
<properties>
    <grpc.version>1.8.0</grpc.version>
</properties>    
```
The following dependencies are required.
```
<dependencies>
    <dependency>
        <groupId>io.grpc</groupId>
        <artifactId>grpc-netty</artifactId>
        <version>${grpc.version}</version>
    </dependency>
    <dependency>
        <groupId>io.grpc</groupId>
        <artifactId>grpc-protobuf</artifactId>
        <version>${grpc.version}</version>
    </dependency>
    <dependency>
        <groupId>io.grpc</groupId>
        <artifactId>grpc-stub</artifactId>
        <version>${grpc.version}</version>
    </dependency>
    <dependency>
        <groupId>io.netty</groupId>
        <artifactId>netty-tcnative-boringssl-static</artifactId>
        <version>2.0.7.Final</version>
    </dependency>
</dependencies>
```
In the build section,  we'll need to configure the following things :
```
<build>
    <extensions>
        <extension>
            <groupId>kr.motd.maven</groupId>
            <artifactId>os-maven-plugin</artifactId>
            <version>1.5.0.Final</version>
        </extension>
    </extensions>
    <plugins>
        <plugin>
            <groupId>org.xolstice.maven.plugins</groupId>
            <artifactId>protobuf-maven-plugin</artifactId>
            <version>0.5.0</version>
            <configuration>
                <protocArtifact>com.google.protobuf:protoc:3.4.0:exe:${os.detected.classifier}</protocArtifact>
                <pluginId>grpc-java</pluginId>
                <pluginArtifact>io.grpc:protoc-gen-grpc-java:${grpc.version}:exe:${os.detected.classifier}</pluginArtifact>
            </configuration>
            <executions>
                <execution>
                    <goals>
                        <goal>compile</goal>
                        <goal>compile-custom</goal>
                    </goals>
                </execution>
            </executions>
        </plugin>
    </plugins>
</build>
```
#### Main.java
```java
import io.grpc.ManagedChannel;
import io.grpc.netty.GrpcSslContexts;
import io.grpc.netty.NettyChannelBuilder;
import io.netty.handler.ssl.SslContext;
import network.lightning.rpc.*;
import network.lightning.rpc.LightningGrpc.LightningBlockingStub;

import javax.net.ssl.SSLException;
import java.io.File;

public class Main {

    private static final String CERT_PATH = "/Users/user/Library/Application Support/Lnd/tls.cert";
    private static final String HOST = "localhost";
    private static final int PORT = 10009;

    public static void main(String... args) throws SSLException {
        SslContext sslContext = GrpcSslContexts.forClient().trustManager(new File(CERT_PATH)).build();
        NettyChannelBuilder channelBuilder = NettyChannelBuilder.forAddress(HOST, PORT);
        ManagedChannel channel = channelBuilder.sslContext(sslContext).build();
        LightningBlockingStub stub = LightningGrpc.newBlockingStub(channel);

        GetInfoResponse response =  stub.getInfo(GetInfoRequest.getDefaultInstance());
        System.out.println(response.getIdentityPubkey());
    }
}
```
#### Running the example
```
mvn compile
```

Run Main.main() in your IDE. 
