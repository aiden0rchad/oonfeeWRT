include $(TOPDIR)/rules.mk

PKG_NAME:=oonfeewrt-agent
PKG_VERSION:=0.1.1
PKG_RELEASE:=1
PKG_LICENSE:=Apache-2.0
PKGARCH:=all

include $(INCLUDE_DIR)/package.mk

define Package/oonfeewrt-agent
  SECTION:=net
  CATEGORY:=Network
  TITLE:=Optional read-only oonfeeWRT rpcd helper
  DEPENDS:=+rpcd +ubus +jshn
  URL:=https://github.com/aiden0rchad/oonfeeWRT
endef

define Package/oonfeewrt-agent/description
  Explicit opt-in, versioned read-only status helper. No background service,
  remote shell, configuration changes, installation, or firmware flashing.
  The default oonfeeWRT controller does not require this package.
endef

define Build/Compile
endef

define Package/oonfeewrt-agent/install
	$(INSTALL_DIR) $(1)/usr/libexec/rpcd $(1)/usr/share/rpcd/acl.d
	$(INSTALL_BIN) ./files/oonfeewrt-agent $(1)/usr/libexec/rpcd/oonfeewrt-agent
	$(INSTALL_DATA) ./files/oonfeewrt-agent.json $(1)/usr/share/rpcd/acl.d/oonfeewrt-agent.json
endef

$(eval $(call BuildPackage,oonfeewrt-agent))
