# JSch (com.github.mwiede:jsch) instancia cifrados, kex, firmas y MACs por
# nombre de clase desde su configuración: hay que conservarlos tal cual.
-keep class com.jcraft.jsch.** { *; }

# Dependencias opcionales de JSch que no existen en Android (Kerberos, JNA,
# Bouncy Castle, sockets Unix, registradores): nunca se cargan.
-dontwarn org.ietf.jgss.**
-dontwarn com.sun.jna.**
-dontwarn org.bouncycastle.**
-dontwarn org.newsclub.**
-dontwarn org.slf4j.**
-dontwarn org.apache.logging.log4j.**
-dontwarn javax.security.auth.**
-dontwarn sun.**
